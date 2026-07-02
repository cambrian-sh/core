package centralexec

import (
	"bytes"
	"errors"
	"testing"
)

// Tracer (ADR-0037 D10, 0037-07): a yielded sub-goal carries opaque
// continuation_state the coordinator stores and returns verbatim on resume —
// the CE never inspects it (keeps the agent's internals private).
func TestYieldCoordinator_ResumeReturnsContinuationVerbatim(t *testing.T) {
	c := NewYieldCoordinator(0.1)
	root := c.OpenRoot([]float32{1, 0, 0})

	state := []byte(`{"step":3,"locals":"opaque"}`)
	child, err := c.Yield(root, SubGoal{
		Intent:            "fetch the exchange rate",
		ContinuationState: state,
	}, []float32{0, 1, 0})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if child == "" {
		t.Fatal("Yield returned empty child id")
	}

	got, ok := c.Resume(root)
	if !ok {
		t.Fatal("Resume: parent had no pending continuation")
	}
	if !bytes.Equal(got, state) {
		t.Errorf("continuation_state = %q, want verbatim %q", got, state)
	}
}

// O(1) ancestry cycle rejection: an agent already in a sub-goal's ancestry can
// never be bound again on that path (ADR-0037 D15 #2, 0037-07).
func TestYieldCoordinator_AncestryCycleRejected(t *testing.T) {
	c := NewYieldCoordinator(0.1)
	root := c.OpenRoot([]float32{1, 0, 0})
	if err := c.BindResource(root, "A"); err != nil {
		t.Fatalf("bind root: %v", err)
	}

	child, err := c.Yield(root, SubGoal{Intent: "sub"}, []float32{0, 1, 0})
	if err != nil {
		t.Fatalf("yield: %v", err)
	}

	if err := c.BindResource(child, "A"); !errors.Is(err, ErrCycle) {
		t.Errorf("binding A (in ancestry) = %v, want ErrCycle", err)
	}
	if err := c.BindResource(child, "B"); err != nil {
		t.Errorf("binding fresh agent B = %v, want nil", err)
	}
}

// Liveness: a sub-goal whose intent is ~identical to its parent (not a strict
// refinement) is terminated by the narrowing guard (ADR-0037 D15 #1, 0037-07).
func TestYieldCoordinator_LivelockGuardRejectsNonRefinement(t *testing.T) {
	c := NewYieldCoordinator(0.1)
	root := c.OpenRoot([]float32{1, 0, 0})

	// Near-identical intent → rejected.
	if _, err := c.Yield(root, SubGoal{Intent: "same thing"}, []float32{1, 0, 0}); !errors.Is(err, ErrLivelock) {
		t.Errorf("near-identical sub-goal = %v, want ErrLivelock", err)
	}
	// A genuinely narrower / distinct intent → accepted.
	if _, err := c.Yield(root, SubGoal{Intent: "a distinct narrower task"}, []float32{0, 0, 1}); err != nil {
		t.Errorf("distinct sub-goal = %v, want nil", err)
	}
}
