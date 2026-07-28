package memory

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// inductionReadStore records reads and can fail on demand.
type inductionReadStore struct {
	fakeVectorStore
	reads atomic.Int32
	err   error
	docs  []domain.Document
}

func (c *inductionReadStore) QueryByMetadata(_ context.Context, _ map[string]string, _ int) ([]domain.Document, error) {
	c.reads.Add(1)
	return c.docs, c.err
}

// Zero interval is the DEFAULT, and default must mean no goroutine, no ticker and no
// store reads at all — not "runs but does nothing".
func TestProcedureScheduler_DisabledByDefault(t *testing.T) {
	store := &inductionReadStore{}
	s := &ProcedureScheduler{
		Inducer: &ProcedureInducer{Store: store, Embedder: &recordingEmbedder{}},
		Store:   store,
		// Interval left zero
	}
	s.Start(context.Background())
	time.Sleep(60 * time.Millisecond)
	if got := store.reads.Load(); got != 0 {
		t.Errorf("a disabled scheduler must not touch the store, got %d reads", got)
	}
}

// A nil scheduler is the shape the kernel holds when the arm is off; Start must be safe.
func TestProcedureScheduler_NilStartIsSafe(t *testing.T) {
	var s *ProcedureScheduler
	s.Start(context.Background()) // must not panic
}

// ADR-0049 A2.5: nothing load-bearing may depend on the pass, so a store error must be
// swallowed and the loop must survive it. A scheduler that died on a transient failure
// would stop producing procedures with no signal beyond their absence.
func TestProcedureScheduler_SurvivesStoreErrors(t *testing.T) {
	store := &inductionReadStore{err: errors.New("transient store failure")}
	s := &ProcedureScheduler{
		Inducer:  &ProcedureInducer{Store: store, Embedder: &recordingEmbedder{}},
		Store:    store,
		Interval: 20 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	time.Sleep(150 * time.Millisecond)
	if got := store.reads.Load(); got < 2 {
		t.Errorf("the loop must keep its cadence through errors, got %d reads", got)
	}
}

// The loop must exit with the kernel rather than outliving it.
func TestProcedureScheduler_StopsOnContextCancel(t *testing.T) {
	store := &inductionReadStore{}
	s := &ProcedureScheduler{
		Inducer:  &ProcedureInducer{Store: store, Embedder: &recordingEmbedder{}},
		Store:    store,
		Interval: 20 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)
	time.Sleep(80 * time.Millisecond)
	cancel()
	time.Sleep(40 * time.Millisecond)
	settled := store.reads.Load()
	time.Sleep(120 * time.Millisecond)
	if after := store.reads.Load(); after != settled {
		t.Errorf("cancelled scheduler kept reading: %d -> %d", settled, after)
	}
}

// Re-running over the same corpus must update in place, not mint a new procedure each
// pass — a nightly job that duplicated its own output would bury the store.
func TestProcedureID_IsStableAcrossPasses(t *testing.T) {
	c := ProcedureCandidate{Signature: "build>deploy", Trigger: "goal: ship a release"}
	first := procedureID(c)
	for i := 0; i < 5; i++ {
		if again := procedureID(c); again != first {
			t.Fatalf("procedure id must be stable: %q vs %q", first, again)
		}
	}
	// Whitespace/case in the trigger must not fork the identity either.
	noisy := ProcedureCandidate{Signature: "build>deploy", Trigger: "Goal:  ship a  release "}
	if procedureID(noisy) != first {
		t.Errorf("trigger normalisation must not fork procedure identity")
	}
	different := ProcedureCandidate{Signature: "build>test>deploy", Trigger: "goal: ship a release"}
	if procedureID(different) == first {
		t.Error("a different capability shape must be a different procedure")
	}
}
