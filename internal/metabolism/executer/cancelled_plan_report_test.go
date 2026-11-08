package executer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ctxRecorder records whether the ctx handed to RecordPlanOutcome was still live.
type ctxRecorder struct {
	outcomes int
	ctxErr   error
}

func (r *ctxRecorder) BeginExperience(context.Context, string) error { return nil }

func (r *ctxRecorder) RecordPlanOutcome(ctx context.Context, _ domain.PlanRecord) error {
	r.outcomes++
	r.ctxErr = ctx.Err()
	return nil
}

// A plan whose ctx ends while it is PAUSED (an operator pause; the caller's deadline
// or cancellation arrives meanwhile) used to return the bare ctx error from inside the
// coordinator loop: no report of the step that had completed, no plan outcome. It now
// ends like every other abort — a PartialPlanError carrying the report, and the plan
// outcome recorded on a ctx that outlives the cancellation
// (T-20261107-a-delegated-plan-stopped-by-its-caller-s).
func TestCancelledWhilePaused_ReturnsTheReportAndRecordsTheOutcome(t *testing.T) {
	rec := &ctxRecorder{}
	ex := &DAGExecutor{MemoryRecorder: rec}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stepFn := func(_ context.Context, i int, _ *domain.Handoff) (*domain.Handoff, error) {
		if i == 0 {
			ex.Pause() // the operator pauses while step 0 runs
			go func() {
				time.Sleep(20 * time.Millisecond)
				cancel() // the caller gives up while the plan waits for resume
			}()
			return &domain.Handoff{Payload: &domain.Payload{Data: []byte("case 42 approved")}}, nil
		}
		t.Errorf("step %d dispatched after the plan was paused", i)
		return nil, errors.New("unexpected")
	}

	done := make(chan error, 1)
	go func() {
		_, err := ex.Execute(ctx, twoStepPlan(), nil, StepFunc(stepFn))
		done <- err
	}()
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a paused plan whose ctx ended never returned")
	}

	var pe *PartialPlanError
	if !errors.As(err, &pe) {
		t.Fatalf("want a PartialPlanError, got %T: %v", err, err)
	}
	if pe.Report != "case 42 approved" {
		t.Errorf("report = %q, want the completed step's result", pe.Report)
	}
	if pe.FailedStep != -1 {
		t.Errorf("FailedStep = %d, want -1 (no step failed; the plan was stopped between steps)", pe.FailedStep)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the cause must stay in the chain: %v", err)
	}
	if rec.outcomes != 1 {
		t.Fatalf("plan outcomes recorded = %d, want 1", rec.outcomes)
	}
	if rec.ctxErr != nil {
		t.Errorf("plan outcome recorded on a cancelled ctx (%v): the write would be dropped", rec.ctxErr)
	}
}
