package centralexec

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// --- fakes ---

type fakeYieldCaller struct {
	calls []*domain.Handoff
	fn    func(n int, agentID string, h *domain.Handoff) (*domain.Handoff, error)
}

func (f *fakeYieldCaller) CallAgent(_ context.Context, agentID string, h *domain.Handoff) (*domain.Handoff, error) {
	f.calls = append(f.calls, h)
	return f.fn(len(f.calls), agentID, h)
}

type fakeBinder struct{ agent string }

func (b fakeBinder) Bind(_ context.Context, _ string) (string, error) { return b.agent, nil }

type mapEmb struct{ m map[string][]float32 }

func (e mapEmb) Embed(_ context.Context, text string) ([]float32, error) {
	if v, ok := e.m[text]; ok {
		return v, nil
	}
	return []float32{0, 0, 1}, nil
}

func yieldResp(intent string) *domain.Handoff {
	return &domain.Handoff{
		Payload: &domain.Payload{},
		Context: map[string]string{"_yield": "true", "_yield_intent": intent},
	}
}

func textResp(s string) *domain.Handoff {
	return &domain.Handoff{Payload: &domain.Payload{Data: []byte(s)}}
}

func driver(caller YieldCaller, binder YieldBinder, emb domain.Embedder, depth int) *YieldDriver {
	return &YieldDriver{Coordinator: NewYieldCoordinator(0.15), Binder: binder, Caller: caller, Embedder: emb, MaxDepth: depth}
}

// Happy path: parent yields → sub-goal bound + dispatched → parent resumed with
// the sub-result → returns the parent's final answer.
func TestYieldDriver_DelegatesAndResumes(t *testing.T) {
	caller := &fakeYieldCaller{}
	caller.fn = func(n int, agentID string, h *domain.Handoff) (*domain.Handoff, error) {
		switch n {
		case 1: // parent's first call → yields a sub-goal
			return yieldResp("sub task"), nil
		case 2: // sub-agent runs → returns a result
			return textResp("SUB_RESULT"), nil
		case 3: // parent resumed — must carry the sub-result
			if h.Context["_yield_result"] != "SUB_RESULT" {
				t.Errorf("resume missing sub-result: %v", h.Context)
			}
			return textResp("FINAL"), nil
		}
		return textResp("?"), nil
	}
	d := driver(caller, fakeBinder{agent: "sub-agent"},
		mapEmb{m: map[string][]float32{"root task": {1, 0, 0}, "sub task": {0, 1, 0}}}, 8)

	h := &domain.Handoff{Payload: &domain.Payload{Data: []byte("root task")}}
	resp, err := d.Drive(context.Background(), "parent", h)
	if err != nil {
		t.Fatalf("Drive: %v", err)
	}
	if subResultText(resp) != "FINAL" {
		t.Errorf("final = %q, want FINAL", subResultText(resp))
	}
	if len(caller.calls) != 3 {
		t.Errorf("expected 3 agent calls (parent, sub, resume), got %d", len(caller.calls))
	}
}

// No yield ⇒ exactly one call, no frontier.
func TestYieldDriver_FastPathNoYield(t *testing.T) {
	caller := &fakeYieldCaller{fn: func(int, string, *domain.Handoff) (*domain.Handoff, error) {
		return textResp("done"), nil
	}}
	d := driver(caller, fakeBinder{agent: "x"}, mapEmb{}, 8)
	resp, err := d.Drive(context.Background(), "a", &domain.Handoff{Payload: &domain.Payload{Data: []byte("t")}})
	if err != nil || subResultText(resp) != "done" {
		t.Fatalf("resp=%q err=%v", subResultText(resp), err)
	}
	if len(caller.calls) != 1 {
		t.Errorf("non-yield must be one call, got %d", len(caller.calls))
	}
}

// Binding a sub-goal back to an ancestor agent is a cycle → ErrCycle.
func TestYieldDriver_CycleRejected(t *testing.T) {
	caller := &fakeYieldCaller{fn: func(n int, _ string, _ *domain.Handoff) (*domain.Handoff, error) {
		if n == 1 {
			return yieldResp("sub task"), nil
		}
		return textResp("x"), nil
	}}
	// binder routes the sub-goal back to the parent agent (already in ancestry).
	d := driver(caller, fakeBinder{agent: "parent"},
		mapEmb{m: map[string][]float32{"root task": {1, 0, 0}, "sub task": {0, 1, 0}}}, 8)
	_, err := d.Drive(context.Background(), "parent",
		&domain.Handoff{Payload: &domain.Payload{Data: []byte("root task")}})
	if !errors.Is(err, ErrCycle) {
		t.Errorf("want ErrCycle, got %v", err)
	}
}

// Re-yielding a near-identical intent trips the D15 narrowing guard → ErrLivelock.
func TestYieldDriver_NarrowingGuard(t *testing.T) {
	caller := &fakeYieldCaller{fn: func(n int, _ string, _ *domain.Handoff) (*domain.Handoff, error) {
		if n == 1 {
			return yieldResp("root task"), nil // same intent as the parent
		}
		return textResp("x"), nil
	}}
	d := driver(caller, fakeBinder{agent: "sub"},
		mapEmb{m: map[string][]float32{"root task": {1, 0, 0}}}, 8)
	_, err := d.Drive(context.Background(), "parent",
		&domain.Handoff{Payload: &domain.Payload{Data: []byte("root task")}})
	if !errors.Is(err, ErrLivelock) {
		t.Errorf("want ErrLivelock, got %v", err)
	}
}

// A yield chain deeper than MaxDepth is bounded → ErrMaxYieldDepth.
func TestYieldDriver_MaxDepth(t *testing.T) {
	caller := &fakeYieldCaller{fn: func(n int, _ string, _ *domain.Handoff) (*domain.Handoff, error) {
		switch n {
		case 1:
			return yieldResp("level a"), nil
		case 2:
			return yieldResp("level b"), nil // sub-agent yields again
		}
		return textResp("x"), nil
	}}
	d := driver(caller, fakeBinder{agent: "sub"},
		mapEmb{m: map[string][]float32{"root": {1, 0, 0}, "level a": {0, 1, 0}, "level b": {0, 0, 1}}}, 1)
	_, err := d.Drive(context.Background(), "parent",
		&domain.Handoff{Payload: &domain.Payload{Data: []byte("root")}})
	if !errors.Is(err, ErrMaxYieldDepth) {
		t.Errorf("want ErrMaxYieldDepth, got %v", err)
	}
}
