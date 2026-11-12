package dispatch

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// T-20261112-yield-sub-goal-on-a-composed-lease-runs-under: a worker's
// capability_missing outcome ends the step as a needs_replan error (not an
// answer), and a step stopped for a repeated identical tool failure runs nothing
// more on its lease.

func capabilityMissingResponse() *domain.Handoff {
	return &domain.Handoff{
		Payload: &domain.Payload{Type: domain.CapabilityMissingResultType},
		Context: map[string]string{
			domain.HandoffKeyCapabilityMissing: "true",
			domain.HandoffKeyCapabilityNeeded:  "fetch the supplier detail pages",
			domain.HandoffKeyCapabilityTool:    "mcp:fetch/fetch",
		},
	}
}

func TestExecute_CapabilityMissingOutcomeIsAReplanError(t *testing.T) {
	caller := &fakeCaller{response: capabilityMissingResponse()}
	d := newDispatcher(&fakeGatekeeper{candidates: []domain.ScoredCandidate{cand("analyst", 0.9)}}, caller)
	in := &domain.Handoff{Context: map[string]string{"_step_index": "1", "_composition_id": "cmp_1"}}
	res, err := d.Execute(context.Background(), &domain.DispatchTask{ID: "task-1"}, in)
	var cm *domain.CapabilityMissingError
	if !errors.As(err, &cm) {
		t.Fatalf("err = %v, want CapabilityMissingError", err)
	}
	if cm.Needed[0] != "fetch the supplier detail pages" || cm.OutsideStep[0] != "mcp:fetch/fetch" || cm.Composition != "cmp_1" {
		t.Fatalf("error %+v", cm)
	}
	if domain.ClassOfStepError(err) != domain.StepFailureNeedsReplan || res == nil || res.Handoff != nil {
		t.Fatalf("class %q res %+v", domain.ClassOfStepError(err), res)
	}
}

func TestCallAgent_CapabilityMissingOutcomeIsAReplanError(t *testing.T) {
	// A resumed worker (yield) or a runner-up reporting it ends the step too.
	d := newDispatcher(&fakeGatekeeper{}, &fakeCaller{response: capabilityMissingResponse()})
	_, err := d.CallAgent(context.Background(), "composed_worker", &domain.Handoff{Context: map[string]string{"task_id": "task-2"}}, "")
	var cm *domain.CapabilityMissingError
	if !errors.As(err, &cm) || cm.TaskID != "task-2" {
		t.Fatalf("err = %v", err)
	}
}

func TestExecute_StoppedStepRunsNothingMore(t *testing.T) {
	f := domain.NewStepFailures(2)
	f.Observe("lease-1", "mcp:filesystem/write_file", []byte(`{"path":"a.csv"}`), "refused")
	f.Observe("lease-1", "mcp:filesystem/write_file", []byte(`{"path":"a.csv"}`), "refused")
	caller := &fakeCaller{}
	d := newDispatcher(&fakeGatekeeper{candidates: []domain.ScoredCandidate{cand("analyst", 0.9)}}, caller)
	d.StepFailures = f
	in := &domain.Handoff{Context: map[string]string{"_step_index": "1", "_session_token_id": "lease-1"}}
	_, err := d.Execute(context.Background(), &domain.DispatchTask{ID: "task-1/yield-1", Subgoal: true}, in)
	var stuck *domain.StepStuckError
	if !errors.As(err, &stuck) || len(caller.called) != 0 {
		t.Fatalf("err=%v called=%v: a stopped step must not be dispatched again", err, caller.called)
	}
}

func TestExecute_WorkerReturningFromAStoppedStepEndsIt(t *testing.T) {
	f := domain.NewStepFailures(2)
	d := newDispatcher(&fakeGatekeeper{candidates: []domain.ScoredCandidate{cand("analyst", 0.9)}}, &fakeCaller{})
	d.Caller = &stoppingCaller{f: f}
	d.StepFailures = f
	in := &domain.Handoff{Context: map[string]string{"_step_index": "0", "_session_token_id": "lease-2"}}
	_, err := d.Execute(context.Background(), &domain.DispatchTask{ID: "task-0"}, in)
	var stuck *domain.StepStuckError
	if !errors.As(err, &stuck) || stuck.Count != 2 {
		t.Fatalf("err = %v: the worker's answer must not stand once the step was stopped", err)
	}
}

// stoppingCaller is a worker that fails the same call twice during its run (the
// executor records it on the lease) and then answers as if all were well.
type stoppingCaller struct{ f *domain.StepFailures }

func (s *stoppingCaller) CallAgent(_ context.Context, agentID string, h *domain.Handoff, _ string) (*domain.Handoff, error) {
	lease := domain.LeaseID(h.Context["_session_token_id"])
	s.f.Observe(lease, "write_file", []byte(`{"path":"x"}`), "file exists")
	s.f.Observe(lease, "write_file", []byte(`{"path":"x"}`), "file exists")
	return &domain.Handoff{FromAgent: agentID, Payload: &domain.Payload{Data: []byte("done")}}, nil
}
