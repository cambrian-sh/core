package executer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// countingReplanner records Replan calls and answers with a one-step plan.
type countingReplanner struct{ calls atomic.Int32 }

func (r *countingReplanner) Replan(ctx context.Context, _ int, _ error, _ map[string]string, _ *domain.ExecutionPlan) (*domain.ExecutionPlan, error) {
	r.calls.Add(1)
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("replan planner call: %w", err) // what the real planner call does
	}
	return &domain.ExecutionPlan{Subject: "replanned", Steps: []domain.Step{{Query: "recover"}}}, nil
}

var errCallerReserve = fmt.Errorf("plan stopped 5s before its caller's deadline to report what it did: %w", context.DeadlineExceeded)

// T-20261108-replanhandler-replans-a-step-that-the-caller: the caller-deadline
// reserve cut the plan; the step it cut must not be replanned on the dead context.
// The plan ends with the partial report of what completed.
func TestDAGExecutor_NoReplanAfterThePlanCeilingFired(t *testing.T) {
	ctx, cancel := context.WithDeadlineCause(t.Context(), time.Now().Add(150*time.Millisecond), errCallerReserve)
	defer cancel()
	plan := &domain.ExecutionPlan{Subject: "s", Steps: []domain.Step{
		{Query: "record the decision"},
		{Query: "draft the reply", DependsOn: []int{0}},
		{Query: "slow lookup"},
	}}
	stepFn := func(sctx context.Context, i int, h *domain.Handoff) (*domain.Handoff, error) {
		if strings.HasPrefix(string(h.Payload.Data), "slow lookup") {
			<-sctx.Done() // runs into the ceiling
			return nil, sctx.Err()
		}
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("done: " + string(h.Payload.Data))}}, nil
	}
	rp := &countingReplanner{}
	_, err := (&DAGExecutor{ReplanHandler: rp, MaxReplanAttempts: 2}).Execute(ctx, plan, nil, stepFn)
	if n := rp.calls.Load(); n != 0 {
		t.Fatalf("Replan called %d times after the plan's ceiling fired; want 0", n)
	}
	var ppe *PartialPlanError
	if !errors.As(err, &ppe) {
		t.Fatalf("err = %v, want the partial-plan report", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the timed-out cause must stay in the chain: %v", err)
	}
	if ppe.FailedStep != 2 || !strings.Contains(ppe.Report, "done: ") {
		t.Errorf("partial report = step %d, %q", ppe.FailedStep, ppe.Report)
	}
}

// Control: a step that fails while the plan is live is still replanned.
func TestDAGExecutor_StepFailureOnALivePlanIsStillReplanned(t *testing.T) {
	plan := &domain.ExecutionPlan{Subject: "s", Steps: []domain.Step{{Query: "flaky"}}}
	stepFn := func(_ context.Context, _ int, h *domain.Handoff) (*domain.Handoff, error) {
		if string(h.Payload.Data) == "flaky" {
			return nil, errors.New("tool crashed")
		}
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("ok")}}, nil
	}
	rp := &countingReplanner{}
	if _, err := (&DAGExecutor{ReplanHandler: rp, MaxReplanAttempts: 2}).Execute(t.Context(), plan, nil, stepFn); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if rp.calls.Load() != 1 {
		t.Fatalf("Replan calls = %d, want 1", rp.calls.Load())
	}
}
