package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

// newReactiveTestAdapter builds a BBoltAdapter with only the reactive buckets, so
// the tests don't run a full agent-dir seed.
func newReactiveTestAdapter(t *testing.T) (*BBoltAdapter, func()) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "reactive_test.db")
	db, err := bbolt.Open(dbPath, 0600, &bbolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		t.Fatalf("open bbolt: %v", err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		for _, b := range [][]byte{reactiveJournalBucket, reactiveCursorBucket, reactiveIdempotencyBucket, reactiveDeadLetterBucket} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("create buckets: %v", err)
	}
	return &BBoltAdapter{db: db}, func() { db.Close(); os.RemoveAll(dir) }
}

// MarkReactiveExecutedOnce is the exactly-once primitive: true exactly once per key.
func TestReactive_MarkExecutedOnce_ExactlyOnce(t *testing.T) {
	a, cleanup := newReactiveTestAdapter(t)
	defer cleanup()

	first, err := a.MarkReactiveExecutedOnce("k1", time.Now())
	if err != nil {
		t.Fatalf("first mark: %v", err)
	}
	if !first {
		t.Fatal("expected first=true on the first mark")
	}
	// Every subsequent call for the same key is false.
	for i := 0; i < 5; i++ {
		again, err := a.MarkReactiveExecutedOnce("k1", time.Now())
		if err != nil {
			t.Fatalf("repeat mark %d: %v", i, err)
		}
		if again {
			t.Fatalf("expected false on repeat mark %d", i)
		}
	}
	// A different key is still first.
	other, err := a.MarkReactiveExecutedOnce("k2", time.Now())
	if err != nil || !other {
		t.Fatalf("expected first=true for a new key, got %v (err %v)", other, err)
	}
}

// Journal append assigns monotonic seqs; ReplayFrom returns entries after a cursor.
func TestReactive_JournalAppendAndReplay(t *testing.T) {
	a, cleanup := newReactiveTestAdapter(t)
	defer cleanup()

	var seqs []uint64
	for i := 0; i < 5; i++ {
		seq, err := a.AppendReactiveSignal(ReactiveJournalRecord{
			StreamID:   "stream-1",
			SignalJSON: []byte(`{"n":` + string(rune('0'+i)) + `}`),
			ReceivedAt: time.Now(),
		})
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		seqs = append(seqs, seq)
	}
	// Monotonic and strictly increasing.
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("seqs not monotonic: %v", seqs)
		}
	}
	// Replay from the 2nd seq returns the last 3.
	recs, err := a.ReplayReactiveFrom(seqs[1])
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("expected 3 records after seq %d, got %d", seqs[1], len(recs))
	}
	if recs[0].Seq != seqs[2] {
		t.Fatalf("expected first replayed seq %d, got %d", seqs[2], recs[0].Seq)
	}
	// Replay from 0 returns everything.
	all, _ := a.ReplayReactiveFrom(0)
	if len(all) != 5 {
		t.Fatalf("expected 5 from replay(0), got %d", len(all))
	}
}

func TestReactive_Cursor(t *testing.T) {
	a, cleanup := newReactiveTestAdapter(t)
	defer cleanup()

	got, err := a.GetReactiveCursor("w1")
	if err != nil || got != 0 {
		t.Fatalf("expected 0 cursor for unknown watch, got %d (err %v)", got, err)
	}
	if err := a.SetReactiveCursor("w1", 42); err != nil {
		t.Fatalf("set cursor: %v", err)
	}
	got, _ = a.GetReactiveCursor("w1")
	if got != 42 {
		t.Fatalf("expected cursor 42, got %d", got)
	}
}

func TestReactive_DeadLetterListNewestFirst(t *testing.T) {
	a, cleanup := newReactiveTestAdapter(t)
	defer cleanup()

	base := time.Now()
	for i := 0; i < 3; i++ {
		if err := a.RecordReactiveDeadLetter(ReactiveDeadLetterRecord{
			ID:       "dl-" + string(rune('a'+i)),
			WatchID:  "w1",
			Reason:   "boom",
			FailedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("record dl %d: %v", i, err)
		}
	}
	all, err := a.ListReactiveDeadLetters(0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 dead-letters, got %d", len(all))
	}
	// Newest-first.
	if !all[0].FailedAt.After(all[1].FailedAt) || !all[1].FailedAt.After(all[2].FailedAt) {
		t.Fatalf("dead-letters not newest-first: %v", all)
	}
	// Limit honored.
	lim, _ := a.ListReactiveDeadLetters(2)
	if len(lim) != 2 {
		t.Fatalf("expected 2 with limit, got %d", len(lim))
	}
}

// Prune removes only records that are BOTH at/below minAcked AND TTL-expired.
func TestReactive_PruneGuardedByAckAndTTL(t *testing.T) {
	a, cleanup := newReactiveTestAdapter(t)
	defer cleanup()

	now := time.Now()
	// seq1: acked + expired  → prunable
	// seq2: acked + live      → kept (still live)
	// seq3: unacked + expired → kept (above minAcked)
	s1, _ := a.AppendReactiveSignal(ReactiveJournalRecord{StreamID: "s", TTLExpires: now.Add(-time.Hour)})
	_, _ = a.AppendReactiveSignal(ReactiveJournalRecord{StreamID: "s", TTLExpires: now.Add(time.Hour)})
	_, _ = a.AppendReactiveSignal(ReactiveJournalRecord{StreamID: "s", TTLExpires: now.Add(-time.Hour)})

	pruned, err := a.PruneReactiveJournal(s1+1 /* minAcked covers seq1,seq2 */, now)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("expected exactly 1 pruned (acked+expired), got %d", pruned)
	}
	remaining, _ := a.ReplayReactiveFrom(0)
	if len(remaining) != 2 {
		t.Fatalf("expected 2 remaining after prune, got %d", len(remaining))
	}
}
