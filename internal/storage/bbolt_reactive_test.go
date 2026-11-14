package storage

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.etcd.io/bbolt"
	"go.uber.org/goleak"
)

// newReactiveTestAdapter builds a BBoltAdapter with only the reactive buckets, so
// the tests don't run a full agent-dir seed.
func newReactiveTestAdapter(t *testing.T) (*BBoltAdapter, func()) {
	t.Helper()
	dir := t.TempDir()
	return openReactiveTestAdapter(t, filepath.Join(dir, "reactive_test.db"))
}

// openReactiveTestAdapter opens (or re-opens) path through the production
// constructor, so bucket creation and the A2.10 migration run exactly as at boot.
func openReactiveTestAdapter(t *testing.T, path string) (*BBoltAdapter, func()) {
	t.Helper()
	a, err := NewBBoltAdapterNoScan(path)
	if err != nil {
		t.Fatalf("open adapter: %v", err)
	}
	return a, func() { a.db.Close() }
}

func appendOn(t *testing.T, a *BBoltAdapter, stream string, ttl time.Time) uint64 {
	t.Helper()
	seq, err := a.AppendReactiveSignal(ReactiveJournalRecord{
		StreamID:   stream,
		SignalJSON: []byte(fmt.Sprintf(`{"stream_id":%q}`, stream)),
		ReceivedAt: time.Now(),
		TTLExpires: ttl,
	})
	if err != nil {
		t.Fatalf("append on %q: %v", stream, err)
	}
	return seq
}

func exact(s string) func(string) bool { return func(x string) bool { return x == s } }

// noConsumer is a floor under which no stream has a consumer (TTL alone governs).
func noConsumer(string) (uint64, bool) { return 0, false }

// floorMap builds a per-stream floor; streams absent from m have no consumer.
func floorMap(m map[string]uint64) func(string) (uint64, bool) {
	return func(s string) (uint64, bool) { f, ok := m[s]; return f, ok }
}

func seqsOf(recs []ReactiveJournalRecord) []uint64 {
	out := make([]uint64, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Seq)
	}
	return out
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

// Seqs are one journal-wide counter across streams (A2.1); ReplayStreams reads
// only the matching streams, after the cursor, merged into seq order.
func TestReactive_JournalAppendAndReplayPerStream(t *testing.T) {
	a, cleanup := newReactiveTestAdapter(t)
	defer cleanup()

	var seqs []uint64
	streams := []string{"a", "b", "a", "c", "b", "a"}
	for _, s := range streams {
		seqs = append(seqs, appendOn(t, a, s, time.Time{}))
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Fatalf("seqs not one global monotonic counter: %v", seqs)
		}
	}

	onA, err := a.ReplayReactiveStreams(exact("a"), 0)
	if err != nil {
		t.Fatalf("replay a: %v", err)
	}
	if got := seqsOf(onA); fmt.Sprint(got) != fmt.Sprint([]uint64{seqs[0], seqs[2], seqs[5]}) {
		t.Fatalf("stream a replay = %v", got)
	}
	for _, r := range onA {
		if r.StreamID != "a" || string(r.SignalJSON) != `{"stream_id":"a"}` {
			t.Fatalf("record not decoded from the bucket name and header: %+v", r)
		}
	}

	// After a cursor: only later records.
	afterA, _ := a.ReplayReactiveStreams(exact("a"), seqs[2])
	if got := seqsOf(afterA); len(got) != 1 || got[0] != seqs[5] {
		t.Fatalf("stream a after %d = %v", seqs[2], got)
	}

	// A multi-stream reader is merged into append order.
	ab, _ := a.ReplayReactiveStreams(func(s string) bool { return s == "a" || s == "b" }, 0)
	want := []uint64{seqs[0], seqs[1], seqs[2], seqs[4], seqs[5]}
	if fmt.Sprint(seqsOf(ab)) != fmt.Sprint(want) {
		t.Fatalf("merged a+b = %v, want %v", seqsOf(ab), want)
	}

	all, _ := a.ReplayReactiveStreams(nil, 0)
	if len(all) != len(streams) {
		t.Fatalf("nil match read %d records, want %d", len(all), len(streams))
	}
	none, _ := a.ReplayReactiveStreams(exact("idle"), 0)
	if len(none) != 0 {
		t.Fatalf("a stream with no bucket returned %d records", len(none))
	}
}

// The empty stream id gets a bucket of its own (bbolt refuses a zero-length name).
func TestReactive_EmptyStreamID(t *testing.T) {
	a, cleanup := newReactiveTestAdapter(t)
	defer cleanup()
	seq := appendOn(t, a, "", time.Time{})
	recs, err := a.ReplayReactiveStreams(exact(""), 0)
	if err != nil || len(recs) != 1 || recs[0].Seq != seq || recs[0].StreamID != "" {
		t.Fatalf("empty-stream replay = %+v (err %v)", recs, err)
	}
}

// The header round-trips and carries the times (A2.3).
func TestReactive_HeaderRoundTrip(t *testing.T) {
	ttl := time.Unix(1750000000, 123).UTC()
	rec := ReactiveJournalRecord{StreamID: "s", SignalJSON: []byte(`{"x":1}`), ReceivedAt: ttl.Add(-time.Hour), TTLExpires: ttl}
	v := encodeJournalValue(rec)
	if len(v) != journalHeaderLen+len(rec.SignalJSON) {
		t.Fatalf("value length %d", len(v))
	}
	got, ok := decodeJournalValue("s", itob(7), v)
	if !ok || got.Seq != 7 || !got.TTLExpires.Equal(ttl) || !got.ReceivedAt.Equal(rec.ReceivedAt) || string(got.SignalJSON) != `{"x":1}` {
		t.Fatalf("round trip = %+v", got)
	}
	zero := encodeJournalValue(ReactiveJournalRecord{})
	if n, _ := journalTTL(zero); n != 0 {
		t.Fatalf("zero ttl encoded as %d", n)
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
	// A2.9: a removed watch's cursor is gone.
	if err := a.DeleteReactiveCursor("w1"); err != nil {
		t.Fatalf("delete cursor: %v", err)
	}
	if got, _ = a.GetReactiveCursor("w1"); got != 0 {
		t.Fatalf("cursor survived its deletion: %d", got)
	}
	if err := a.DeleteReactiveCursor("never-existed"); err != nil {
		t.Fatalf("deleting an absent cursor errored: %v", err)
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

// Prune removes only records that are BOTH at/below their stream's floor AND
// TTL-expired, and the walk stops at the first live record (A2.4).
func TestReactive_PruneGuardedByFloorAndTTL(t *testing.T) {
	a, cleanup := newReactiveTestAdapter(t)
	defer cleanup()

	now := time.Now()
	// seq1: acked + expired  → prunable
	// seq2: acked + live      → kept (still live), and the walk stops here
	// seq3: acked + expired   → kept (behind a live record)
	// seq4: unacked + expired → kept (above the floor)
	s1 := appendOn(t, a, "s", now.Add(-time.Hour))
	appendOn(t, a, "s", now.Add(time.Hour))
	s3 := appendOn(t, a, "s", now.Add(-time.Hour))
	appendOn(t, a, "s", now.Add(-time.Hour))

	pruned, more, err := a.PruneReactiveJournal(floorMap(map[string]uint64{"s": s3}), now, 0)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 1 || more {
		t.Fatalf("pruned=%d more=%v, want exactly seq %d", pruned, more, s1)
	}
	remaining, _ := a.ReplayReactiveStreams(nil, 0)
	if len(remaining) != 3 {
		t.Fatalf("expected 3 remaining after prune, got %d", len(remaining))
	}
}

// THE defect (2026-11-02): an idle watch on one stream must not hold another
// stream's records. Each stream is governed by its own consumers' floor.
func TestReactive_PrunePerStreamFloor(t *testing.T) {
	a, cleanup := newReactiveTestAdapter(t)
	defer cleanup()
	past := time.Now().Add(-time.Hour)

	// The idle stream got 2 records early on; its watch acked neither (cursor 0).
	appendOn(t, a, "idle", past)
	appendOn(t, a, "idle", past)
	var busyLast uint64
	for i := 0; i < 10; i++ {
		busyLast = appendOn(t, a, "busy", past)
	}

	// The A1.2 global floor would be min(0, busyLast) = 0: nothing prunable.
	floor := floorMap(map[string]uint64{"idle": 0, "busy": busyLast})
	pruned, _, err := a.PruneReactiveJournal(floor, time.Now(), 0)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 10 {
		t.Fatalf("pruned %d, want the 10 busy-stream records the busy watch acked", pruned)
	}
	idle, _ := a.ReplayReactiveStreams(exact("idle"), 0)
	if len(idle) != 2 {
		t.Fatalf("the un-acked idle-stream records were pruned: %d left", len(idle))
	}
	// The emptied stream bucket is gone; the window reports only what remains.
	lo, hi, n, _ := a.ReactiveJournalWindow(nil)
	if n != 2 || lo != 1 || hi != 2 {
		t.Fatalf("window = [%d,%d] x%d, want [1,2] x2", lo, hi, n)
	}
}

// A stream no registered watch consumes is governed by TTL alone (A2.2), and a
// live record on it is still kept.
func TestReactive_PruneStreamWithNoConsumer(t *testing.T) {
	a, cleanup := newReactiveTestAdapter(t)
	defer cleanup()
	now := time.Now()
	for i := 0; i < 5; i++ {
		appendOn(t, a, "orphan", now.Add(-time.Minute))
	}
	appendOn(t, a, "orphan", now.Add(time.Hour))
	pruned, _, err := a.PruneReactiveJournal(noConsumer, now, 0)
	if err != nil || pruned != 5 {
		t.Fatalf("pruned=%d err=%v, want the 5 expired records", pruned, err)
	}
	if _, _, n, _ := a.ReactiveJournalWindow(exact("orphan")); n != 1 {
		t.Fatalf("the live record went too: %d left", n)
	}
}

// Bounded across streams (A1.1): the cap holds over the whole pass and More says
// a prunable record was left.
func TestReactive_PruneBoundedAcrossStreams(t *testing.T) {
	a, cleanup := newReactiveTestAdapter(t)
	defer cleanup()
	past := time.Now().Add(-time.Hour)
	for i := 0; i < 30; i++ {
		appendOn(t, a, fmt.Sprintf("s%d", i%3), past)
	}
	total := 0
	for pass := 0; pass < 10; pass++ {
		pruned, more, err := a.PruneReactiveJournal(noConsumer, time.Now(), 7)
		if err != nil {
			t.Fatalf("prune: %v", err)
		}
		if pruned > 7 {
			t.Fatalf("pass pruned %d past the cap of 7", pruned)
		}
		total += pruned
		if !more {
			break
		}
	}
	if total != 30 {
		t.Fatalf("drained %d of 30", total)
	}
	if _, _, n, _ := a.ReactiveJournalWindow(nil); n != 0 {
		t.Fatalf("%d records left after the drain", n)
	}
}

// A2.6: idempotency markers older than the cutoff are pruned; younger ones, and
// markers of unknown age, are kept.
func TestReactive_PruneIdempotency(t *testing.T) {
	a, cleanup := newReactiveTestAdapter(t)
	defer cleanup()
	now := time.Now()
	for i := 0; i < 5; i++ {
		_, _ = a.MarkReactiveExecutedOnce(fmt.Sprintf("old-%d", i), now.Add(-10*24*time.Hour))
	}
	_, _ = a.MarkReactiveExecutedOnce("young", now.Add(-time.Hour))
	if err := a.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(reactiveIdempotencyBucket).Put([]byte("garbled"), []byte("not a time"))
	}); err != nil {
		t.Fatal(err)
	}

	cutoff := now.Add(-7 * 24 * time.Hour)
	pruned, more, err := a.PruneReactiveIdempotency(cutoff, 3)
	if err != nil || pruned != 3 || !more {
		t.Fatalf("capped pass: pruned=%d more=%v err=%v", pruned, more, err)
	}
	pruned, more, _ = a.PruneReactiveIdempotency(cutoff, 3)
	if pruned != 2 || more {
		t.Fatalf("second pass: pruned=%d more=%v", pruned, more)
	}
	// The young marker still dedups; the garbled one survives.
	if first, _ := a.MarkReactiveExecutedOnce("young", now); first {
		t.Fatal("a marker younger than the retention was dropped")
	}
	if first, _ := a.MarkReactiveExecutedOnce("garbled", now); first {
		t.Fatal("a marker of unknown age was dropped")
	}
	if first, _ := a.MarkReactiveExecutedOnce("old-0", now); !first {
		t.Fatal("an expired marker was not pruned")
	}
}

// A2.7: concurrent appends share transactions but every one gets a distinct seq,
// is durable when it returns, and Batch leaves no goroutine behind.
func TestReactive_ConcurrentAppendsAreDistinctAndDurable(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	path := filepath.Join(t.TempDir(), "concurrent.db")
	a, cleanup := openReactiveTestAdapter(t, path)
	const producers, each = 16, 50
	var mu sync.Mutex
	seen := map[uint64]bool{}
	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				seq, err := a.AppendReactiveSignal(ReactiveJournalRecord{StreamID: fmt.Sprintf("p%d", p%4), SignalJSON: []byte(`{}`)})
				if err != nil {
					t.Errorf("append: %v", err)
					return
				}
				mu.Lock()
				if seen[seq] {
					t.Errorf("seq %d assigned twice", seq)
				}
				seen[seq] = true
				mu.Unlock()
			}
		}(p)
	}
	wg.Wait()
	cleanup()

	b, cleanup2 := openReactiveTestAdapter(t, path)
	defer cleanup2()
	all, _ := b.ReplayReactiveStreams(nil, 0)
	if len(all) != producers*each {
		t.Fatalf("%d records after reopen, want %d", len(all), producers*each)
	}
	for i := 1; i < len(all); i++ {
		if all[i].Seq <= all[i-1].Seq {
			t.Fatalf("replay not in seq order at %d", i)
		}
	}
}

// ── A2.10 migration ────────────────────────────────────────────────────────────

// writeLegacy fills the pre-A2 flat bucket exactly as the old AppendReactiveSignal
// did: seq from the bucket's NextSequence, the whole record as JSON.
func writeLegacy(t *testing.T, path string, streams []string, ttl time.Time) []uint64 {
	t.Helper()
	db, err := bbolt.Open(path, 0600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var seqs []uint64
	if err := db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(reactiveJournalLegacyBucket)
		if err != nil {
			return err
		}
		for _, s := range streams {
			n, _ := b.NextSequence()
			data, _ := json.Marshal(ReactiveJournalRecord{
				Seq: n, StreamID: s, SignalJSON: []byte(fmt.Sprintf(`{"n":%d}`, n)),
				ReceivedAt: time.Now(), TTLExpires: ttl,
			})
			if err := b.Put(itob(n), data); err != nil {
				return err
			}
			seqs = append(seqs, n)
		}
		// An undecodable legacy record: the old reader skipped it.
		n, _ := b.NextSequence()
		return b.Put(itob(n), []byte("{not json"))
	}); err != nil {
		t.Fatal(err)
	}
	return seqs
}

func legacyPresent(t *testing.T, a *BBoltAdapter) bool {
	t.Helper()
	present := false
	_ = a.db.View(func(tx *bbolt.Tx) error { present = tx.Bucket(reactiveJournalLegacyBucket) != nil; return nil })
	return present
}

// The migration preserves every record, its seq, its stream and its order; drops
// only the undecodable one; deletes the legacy bucket; and keeps the seq counter
// above every legacy seq, so a cursor is never passed by a reused seq.
func TestReactive_MigrationPreservesRecordsAndOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	ttl := time.Now().Add(time.Hour).UTC()
	streams := []string{"x", "y", "x", "", "y", "x"}
	seqs := writeLegacy(t, path, streams, ttl)

	a, cleanup := openReactiveTestAdapter(t, path)
	defer cleanup()
	if legacyPresent(t, a) {
		t.Fatal("legacy bucket survived the migration")
	}
	all, err := a.ReplayReactiveStreams(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(seqsOf(all)) != fmt.Sprint(seqs) {
		t.Fatalf("migrated seqs %v, want %v", seqsOf(all), seqs)
	}
	for i, r := range all {
		if r.StreamID != streams[i] || string(r.SignalJSON) != fmt.Sprintf(`{"n":%d}`, seqs[i]) || !r.TTLExpires.Equal(ttl) {
			t.Fatalf("record %d migrated wrong: %+v", i, r)
		}
	}
	onX, _ := a.ReplayReactiveStreams(exact("x"), 0)
	if len(onX) != 3 {
		t.Fatalf("stream x has %d records after migration, want 3", len(onX))
	}
	// The undecodable legacy record held seq len(seqs)+1; the next append is above it.
	next := appendOn(t, a, "x", time.Time{})
	if next <= uint64(len(seqs)+1) {
		t.Fatalf("post-migration seq %d reuses a legacy seq (max %d)", next, len(seqs)+1)
	}
}

// Idempotent: re-opening a migrated file changes nothing, and a migration that
// stopped half-way (records in both buckets) completes without duplicating or
// losing any.
func TestReactive_MigrationIsIdempotentAndResumable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	streams := make([]string, 25)
	for i := range streams {
		streams[i] = fmt.Sprintf("s%d", i%4)
	}
	seqs := writeLegacy(t, path, streams, time.Time{})

	// Simulate a crash after the first batch: move the first 10 by hand, the way
	// one migration transaction does (put in v2, delete from legacy).
	db, err := bbolt.Open(path, 0600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		legacy := tx.Bucket(reactiveJournalLegacyBucket)
		parent, _ := tx.CreateBucketIfNotExists(reactiveJournalBucket)
		_ = parent.SetSequence(legacy.Sequence())
		for _, s := range seqs[:10] {
			var rec ReactiveJournalRecord
			_ = json.Unmarshal(legacy.Get(itob(s)), &rec)
			sb, _ := parent.CreateBucketIfNotExists(streamBucketName(rec.StreamID))
			_ = sb.Put(itob(s), encodeJournalValue(rec))
			_ = legacy.Delete(itob(s))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	for open := 0; open < 3; open++ {
		a, cleanup := openReactiveTestAdapter(t, path)
		all, _ := a.ReplayReactiveStreams(nil, 0)
		if fmt.Sprint(seqsOf(all)) != fmt.Sprint(seqs) {
			cleanup()
			t.Fatalf("open %d: seqs %v, want %v", open, seqsOf(all), seqs)
		}
		if legacyPresent(t, a) {
			cleanup()
			t.Fatalf("open %d: legacy bucket still present", open)
		}
		cleanup()
	}
}

// The migration runs in bounded transactions: a legacy bucket larger than one
// batch still migrates completely.
func TestReactive_MigrationSpansBatches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	streams := make([]string, migrateBatch+5)
	for i := range streams {
		streams[i] = "s"
	}
	writeLegacy(t, path, streams, time.Time{})
	a, cleanup := openReactiveTestAdapter(t, path)
	defer cleanup()
	if _, _, n, _ := a.ReactiveJournalWindow(nil); n != len(streams) {
		t.Fatalf("migrated %d of %d", n, len(streams))
	}
}

// ── A2.8 offline compaction ────────────────────────────────────────────────────

func fillAndDrain(t *testing.T, path string, n int) {
	t.Helper()
	a, cleanup := openReactiveTestAdapter(t, path)
	defer cleanup()
	past := time.Now().Add(-time.Hour)
	payload := make([]byte, 900)
	for i := range payload {
		payload[i] = 'a'
	}
	if err := a.db.Update(func(tx *bbolt.Tx) error {
		sb, _ := tx.Bucket(reactiveJournalBucket).CreateBucketIfNotExists([]byte("flood"))
		for i := 1; i <= n; i++ {
			if err := sb.Put(itob(uint64(i)), encodeJournalValue(ReactiveJournalRecord{SignalJSON: payload, TTLExpires: past})); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.PruneReactiveJournal(noConsumer, time.Now(), 0); err != nil {
		t.Fatal(err)
	}
}

func TestCompact_ShrinksABloatedFileAndKeepsThePrevious(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.db")
	fillAndDrain(t, path, 5000)
	before, _ := os.Stat(path)

	// Above the free-ratio threshold but under the size threshold: untouched.
	rep, err := CompactBBoltFileIfBloated(path, before.Size()*2, 0.5)
	if err != nil || rep.Compacted {
		t.Fatalf("compacted a file under the size threshold: %+v (err %v)", rep, err)
	}
	if rep.FreeRatio <= 0.5 {
		t.Fatalf("a drained file reported free ratio %.2f", rep.FreeRatio)
	}
	// Disabled.
	if rep, _ := CompactBBoltFileIfBloated(path, 0, 0.5); rep.Compacted {
		t.Fatal("compact_min_bytes 0 must disable compaction")
	}

	rep, err = CompactBBoltFileIfBloated(path, 1024, 0.5)
	if err != nil || !rep.Compacted {
		t.Fatalf("did not compact: %+v (err %v)", rep, err)
	}
	if rep.AfterBytes >= before.Size()/2 {
		t.Fatalf("compaction %d -> %d B did not shrink the file", before.Size(), rep.AfterBytes)
	}
	if st, err := os.Stat(path + PreCompactSuffix); err != nil || st.Size() != before.Size() {
		t.Fatalf("previous file not kept as %s: %v", PreCompactSuffix, err)
	}
	// The compacted file is a working store with its buckets and sequence intact.
	a, cleanup := openReactiveTestAdapter(t, path)
	if seq := appendOn(t, a, "s", time.Time{}); seq == 0 {
		t.Fatal("append on the compacted store failed")
	}
	cleanup()

	removed, err := RemoveStalePreCompact(path)
	if err != nil || !removed {
		t.Fatalf("pre-compact not removed: %v %v", removed, err)
	}
	if removed, _ := RemoveStalePreCompact(path); removed {
		t.Fatal("removed a pre-compact file that did not exist")
	}
}

// A mostly-live file is not rewritten.
func TestCompact_LeavesALiveFileAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.db")
	a, cleanup := openReactiveTestAdapter(t, path)
	// One transaction: this checks the free-ratio arithmetic, not append cost.
	if err := a.db.Update(func(tx *bbolt.Tx) error {
		sb, _ := tx.Bucket(reactiveJournalBucket).CreateBucketIfNotExists([]byte("s"))
		for i := 1; i <= 2000; i++ {
			if err := sb.Put(itob(uint64(i)), encodeJournalValue(ReactiveJournalRecord{SignalJSON: []byte(`{"live":true}`)})); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cleanup()
	rep, err := CompactBBoltFileIfBloated(path, 1024, 0.9)
	if err != nil || rep.Compacted {
		t.Fatalf("compacted a live file: %+v (err %v)", rep, err)
	}
}

// TestReactive_KeysSortBySeq pins the big-endian key encoding the prefix walk relies on.
func TestReactive_KeysSortBySeq(t *testing.T) {
	if binary.BigEndian.Uint64(itob(258)) != 258 || string(itob(1)) >= string(itob(256)) {
		t.Fatal("itob keys do not sort in seq order")
	}
}

// ENG-472: dead letters get a retention (prune by age, bounded per pass) and an
// operator delete by id.
func TestReactive_DeadLetterPruneAndDelete(t *testing.T) {
	a, cleanup := newReactiveTestAdapter(t)
	defer cleanup()
	now := time.Now()
	for i, age := range []time.Duration{40 * 24 * time.Hour, 35 * 24 * time.Hour, 31 * 24 * time.Hour, time.Hour} {
		if err := a.RecordReactiveDeadLetter(ReactiveDeadLetterRecord{
			ID: "dl-" + string(rune('a'+i)), WatchID: "w1", Reason: "boom", FailedAt: now.Add(-age),
		}); err != nil {
			t.Fatal(err)
		}
	}
	cutoff := now.Add(-30 * 24 * time.Hour)
	removed, more, err := a.PruneReactiveDeadLetters(cutoff, 2)
	if err != nil || removed != 2 || !more {
		t.Fatalf("bounded pass: removed=%d more=%v err=%v", removed, more, err)
	}
	removed, more, err = a.PruneReactiveDeadLetters(cutoff, 2)
	if err != nil || removed != 1 || more {
		t.Fatalf("second pass: removed=%d more=%v err=%v", removed, more, err)
	}
	left, _ := a.ListReactiveDeadLetters(0)
	if len(left) != 1 || left[0].ID != "dl-d" {
		t.Fatalf("only the recent entry must remain: %+v", left)
	}
	if found, err := a.DeleteReactiveDeadLetter("dl-d"); err != nil || !found {
		t.Fatalf("delete: %v %v", found, err)
	}
	if found, err := a.DeleteReactiveDeadLetter("dl-d"); err != nil || found {
		t.Fatalf("a second delete finds nothing: %v %v", found, err)
	}
}
