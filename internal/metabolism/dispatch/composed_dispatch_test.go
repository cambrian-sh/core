package dispatch

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0145 D12: with a Composer, a plan step selected for a profile agent is called
// on the composed runtime; a code/service agent, and a non-step dispatch, are not.

type fakeComposer struct {
	calls int
	ok    bool
}

func (f *fakeComposer) Compose(_ context.Context, _ *domain.DispatchTask, w domain.ScoredCandidate,
	_ []domain.ScoredCandidate, in *domain.Handoff) (domain.ScoredCandidate, domain.WorkerComposition, bool, error) {
	f.calls++
	if !f.ok || w.Agent.Profile == "" {
		return domain.ScoredCandidate{}, domain.WorkerComposition{}, false, nil
	}
	in.Context[domain.HandoffKeyComposition] = "{}"
	return domain.ScoredCandidate{Agent: domain.AgentDefinition{ID: "composed_worker", SourceHash: "rt"}, Score: w.Score},
		domain.WorkerComposition{Runtime: "composed_worker"}, true, nil
}

func profiled(id string, score float64) domain.ScoredCandidate {
	c := cand(id, score)
	c.Agent.Profile = id
	return c
}

func TestComposedDispatch_StepRunsOnTheRuntime_WithNoRunnerUps(t *testing.T) {
	gk := &fakeGatekeeper{candidates: []domain.ScoredCandidate{profiled("researcher", 0.9), profiled("assistant", 0.5)}}
	caller := &fakeCaller{}
	bus := &capturingBus{}
	d := newDispatcher(gk, caller)
	d.EventBus = bus
	d.Composer = &fakeComposer{ok: true}
	in := &domain.Handoff{Context: map[string]string{"_step_index": "0"}}
	res, err := d.Execute(context.Background(), &domain.DispatchTask{ID: "task-0"}, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(caller.called) != 1 || caller.called[0] != "composed_worker" {
		t.Fatalf("called %v, want the composed runtime", caller.called)
	}
	if len(res.RunnerUps) != 0 {
		t.Fatalf("a composed step must not fall back to pre-built agents: %v", res.RunnerUps)
	}
	if in.Context["_selection_reason"] != ReasonComposed || res.Handoff.Context[domain.HandoffKeyWinnerAgentID] != "composed_worker" {
		t.Fatalf("reason=%q winner=%q", in.Context["_selection_reason"], res.Handoff.Context[domain.HandoffKeyWinnerAgentID])
	}
	last := bus.events[len(bus.events)-1]
	if last.WinnerID != "composed_worker" {
		t.Fatalf("selection event winner = %q", last.WinnerID)
	}
}

func TestComposedDispatch_CodeAgentAndNonStepStayOnTheSelectedAgent(t *testing.T) {
	gk := &fakeGatekeeper{candidates: []domain.ScoredCandidate{cand("docling_agent", 0.9)}}
	caller := &fakeCaller{}
	d := newDispatcher(gk, caller)
	comp := &fakeComposer{ok: true}
	d.Composer = comp
	if _, err := d.Execute(context.Background(), &domain.DispatchTask{ID: "t"},
		&domain.Handoff{Context: map[string]string{"_step_index": "0"}}); err != nil {
		t.Fatal(err)
	}
	gk.candidates = []domain.ScoredCandidate{profiled("researcher", 0.9)}
	if _, err := d.Execute(context.Background(), &domain.DispatchTask{ID: "t"}, &domain.Handoff{Context: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	if len(caller.called) != 2 || caller.called[0] != "docling_agent" || caller.called[1] != "researcher" {
		t.Fatalf("called %v, want [docling_agent researcher]", caller.called)
	}
	if comp.calls != 1 {
		t.Fatalf("composer consulted %d times, want 1 (the non-step dispatch is never composed)", comp.calls)
	}
}
