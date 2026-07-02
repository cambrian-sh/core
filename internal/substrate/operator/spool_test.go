package operator_test

import (
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/substrate/operator"
)

// Cycle 1 (tracer) — Emit assigns sequence numbers 1,2,3… and ReadFrom(0)
// returns the events in ascending Seq order.
func TestSpool_EmitSequencesAndReadFromZeroReturnsAll(t *testing.T) {
	s := operator.NewSpool(operator.SpoolConfig{})

	seq1 := s.Emit(domain.AgentReadyEvent{AgentID: "a"})
	seq2 := s.Emit(domain.AgentReadyEvent{AgentID: "b"})
	seq3 := s.Emit(domain.AgentReadyEvent{AgentID: "c"})

	if seq1 != 1 || seq2 != 2 || seq3 != 3 {
		t.Fatalf("expected seqs 1,2,3, got %d,%d,%d", seq1, seq2, seq3)
	}

	got, resync := s.ReadFrom(0)
	if resync {
		t.Fatal("ReadFrom(0) with a fresh spool should not require resync")
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 events, got %d", len(got))
	}
	for i, want := range []uint64{1, 2, 3} {
		if got[i].Seq != want {
			t.Fatalf("event %d: expected Seq %d, got %d", i, want, got[i].Seq)
		}
	}
}

// Cycle 2 — Concurrent Emit assigns strictly unique sequence numbers (the single
// sequencer holds under contention). Run with -race.
func TestSpool_ConcurrentEmitAssignsUniqueSeqs(t *testing.T) {
	s := operator.NewSpool(operator.SpoolConfig{MaxEvents: 10000})

	const n = 500
	var wg sync.WaitGroup
	seqs := make([]uint64, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			seqs[i] = s.Emit(domain.AgentReadyEvent{})
		}(i)
	}
	wg.Wait()

	seen := make(map[uint64]bool, n)
	var max uint64
	for _, sq := range seqs {
		if sq == 0 {
			t.Fatal("Emit returned seq 0")
		}
		if seen[sq] {
			t.Fatalf("duplicate seq %d assigned", sq)
		}
		seen[sq] = true
		if sq > max {
			max = sq
		}
	}
	if len(seen) != n || max != n {
		t.Fatalf("expected %d unique seqs ending at %d, got %d unique ending at %d", n, n, len(seen), max)
	}
}

// Cycle 3 — ReadFrom(cursor) returns only events with Seq > cursor.
func TestSpool_ReadFromCursorReturnsOnlyNewer(t *testing.T) {
	s := operator.NewSpool(operator.SpoolConfig{})
	for i := 0; i < 5; i++ {
		s.Emit(domain.AgentReadyEvent{})
	}

	got, resync := s.ReadFrom(3)
	if resync {
		t.Fatal("cursor 3 is within the window; should not resync")
	}
	if len(got) != 2 || got[0].Seq != 4 || got[1].Seq != 5 {
		t.Fatalf("expected events 4,5; got %+v", seqsOf(got))
	}
}

// Cycle 4 — When the count cap evicts the oldest events, a cursor below the
// retained tail reports resync (and returns no partial/gapped slice).
func TestSpool_CountCapEvictionTriggersResync(t *testing.T) {
	s := operator.NewSpool(operator.SpoolConfig{MaxEvents: 2})
	for i := 0; i < 5; i++ { // events 1..5; only 4,5 retained
		s.Emit(domain.AgentReadyEvent{})
	}

	// A fresh client (cursor 0) cannot be served the evicted 1..3 — resync.
	got, resync := s.ReadFrom(0)
	if !resync {
		t.Fatal("expected resync after count-cap eviction past cursor 0")
	}
	if got != nil {
		t.Fatalf("resync must return nil events, got %+v", seqsOf(got))
	}

	// A client current to seq 4 is still served the retained tail (5).
	got, resync = s.ReadFrom(4)
	if resync {
		t.Fatal("cursor 4 is within the retained window; should not resync")
	}
	if len(got) != 1 || got[0].Seq != 5 {
		t.Fatalf("expected event 5; got %+v", seqsOf(got))
	}
}

// Cycle 5 — Events older than the age window are evicted (injected clock).
func TestSpool_AgeCapEvictsOldEvents(t *testing.T) {
	s := operator.NewSpool(operator.SpoolConfig{MaxAge: 100 * time.Millisecond, MaxEvents: 1000})
	cur := time.Unix(0, 0)
	s.SetClock(func() time.Time { return cur })

	s.Emit(domain.AgentReadyEvent{}) // seq 1 at t=0
	cur = cur.Add(200 * time.Millisecond)
	s.Emit(domain.AgentReadyEvent{}) // seq 2 at t=200ms; eviction drops seq 1 (older than 100ms window)

	// A fresh client cannot be served the aged-out seq 1 — resync.
	if _, resync := s.ReadFrom(0); !resync {
		t.Fatal("expected resync: seq 1 should have aged out of the window")
	}
	// The in-window seq 2 is still served.
	got, resync := s.ReadFrom(1)
	if resync {
		t.Fatal("cursor 1 is within the window; should not resync")
	}
	if len(got) != 1 || got[0].Seq != 2 {
		t.Fatalf("expected only event 2 retained; got %+v", seqsOf(got))
	}
}

// Cycle 6 — A caller already current (cursor >= latest seq) gets no events and
// no resync, even at the boundary.
func TestSpool_CurrentCallerGetsNothingNoResync(t *testing.T) {
	s := operator.NewSpool(operator.SpoolConfig{})
	s.Emit(domain.AgentReadyEvent{})
	s.Emit(domain.AgentReadyEvent{}) // latest seq = 2

	for _, cursor := range []uint64{2, 3, 99} { // at and beyond the head
		got, resync := s.ReadFrom(cursor)
		if resync {
			t.Fatalf("cursor %d (>= head) must not resync", cursor)
		}
		if got != nil {
			t.Fatalf("cursor %d (>= head) must return no events; got %+v", cursor, seqsOf(got))
		}
	}
}

// Cycle 7 — Emit never blocks on (and is never bounded by) a reader: with no one
// draining, Emitting far past the cap completes and the spool stays bounded.
func TestSpool_EmitNeverBlocksAndStaysBounded(t *testing.T) {
	const cap = 8
	s := operator.NewSpool(operator.SpoolConfig{MaxEvents: cap})

	for i := 0; i < cap*100; i++ { // no reader ever drains
		s.Emit(domain.AgentReadyEvent{})
	}

	got, resync := s.ReadFrom(0)
	if !resync {
		t.Fatal("expected resync: a fresh cursor cannot see events evicted by the cap")
	}
	_ = got
	// The retained window stays bounded to the cap regardless of emit volume.
	got, _ = s.ReadFrom(cap*100 - cap) // cursor just below the retained tail
	if len(got) > cap {
		t.Fatalf("retained window exceeded cap: %d > %d", len(got), cap)
	}
}

// Cycle 8 — A reader waiting on Updates() captured before an Emit is woken by
// that Emit (no missed wakeup for an event racing the wait).
func TestSpool_UpdatesWakesWaiterOnEmit(t *testing.T) {
	s := operator.NewSpool(operator.SpoolConfig{})

	ch := s.Updates() // captured before any Emit
	woken := make(chan struct{})
	go func() {
		<-ch
		close(woken)
	}()

	s.Emit(domain.AgentReadyEvent{})

	select {
	case <-woken:
	case <-time.After(time.Second):
		t.Fatal("waiter on Updates() was not woken by Emit")
	}
}

func seqsOf(evs []domain.SequencedEvent) []uint64 {
	out := make([]uint64, len(evs))
	for i, e := range evs {
		out[i] = e.Seq
	}
	return out
}
