package network

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
	"github.com/cambrian-sh/core/internal/substrate/harness"
)

// noopRestorer satisfies harness.Restorer with a no-op Restore.
type noopRestorer struct{}

func (n *noopRestorer) Restore(_, _ string) error { return nil }

// healedDAGStep adapts a harness.StepFunc into a DAGExecutor StepFunc by
// ignoring the step index (the harness closure already captures it).
func healedDAGStep(fn harness.StepFunc) executer.StepFunc {
	return func(ctx context.Context, _ int, h *domain.Handoff) (*domain.Handoff, error) {
		return fn(ctx, h)
	}
}

// ── Cycle 1: step fails once then succeeds ────────────────────────────────────

func TestHealedStep_FailOnceThenSucceed(t *testing.T) {
	calls := 0
	inner := harness.StepFunc(func(_ context.Context, _ *domain.Handoff) (*domain.Handoff, error) {
		calls++
		if calls == 1 {
			return nil, fmt.Errorf("transient failure")
		}
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("ok")}}, nil
	})

	healer := &harness.SelfHealer{Restorer: &noopRestorer{}, StepIndex: 0}
	wrapped := healer.Wrap(inner)

	plan := &domain.ExecutionPlan{Steps: []domain.Step{{Query: "test step"}}}
	result, err := (&executer.DAGExecutor{}).Execute(t.Context(), plan, nil, healedDAGStep(wrapped))

	if err != nil {
		t.Fatalf("expected success after retry, got: %v", err)
	}
	if calls != 2 {
		t.Errorf("expected 2 inner calls (1 fail + 1 retry), got %d", calls)
	}
	if result["step_0_result"] != "ok" {
		t.Errorf("unexpected result: %q", result["step_0_result"])
	}
}

// ── Cycle 2: three failures → HealingExhaustedError propagates ───────────────

func TestHealedStep_ThreeFailures_ReturnsExhaustedError(t *testing.T) {
	calls := 0
	inner := harness.StepFunc(func(_ context.Context, _ *domain.Handoff) (*domain.Handoff, error) {
		calls++
		// Vary message each call so loop detection does not fire early.
		return nil, fmt.Errorf("persistent failure attempt %d", calls)
	})

	healer := &harness.SelfHealer{Restorer: &noopRestorer{}, StepIndex: 1}
	wrapped := healer.Wrap(inner)

	plan := &domain.ExecutionPlan{Steps: []domain.Step{{Query: "always fails"}}}
	_, err := (&executer.DAGExecutor{}).Execute(t.Context(), plan, nil, healedDAGStep(wrapped))

	if err == nil {
		t.Fatal("expected HealingExhaustedError, got nil")
	}
	var healErr *harness.HealingExhaustedError
	if !errors.As(err, &healErr) {
		t.Fatalf("expected *HealingExhaustedError, got %T: %v", err, err)
	}
	if healErr.AttemptCount != 3 {
		t.Errorf("expected AttemptCount=3, got %d", healErr.AttemptCount)
	}
	if healErr.StepIndex != 1 {
		t.Errorf("expected StepIndex=1, got %d", healErr.StepIndex)
	}
	if calls != 3 {
		t.Errorf("expected 3 inner calls, got %d", calls)
	}
}

// ── Cycle 3: LogicError injects _heal_error and _heal_attempt on retry ───────

func TestHealedStep_LogicError_InjectsHealContext(t *testing.T) {
	var capturedContexts []map[string]string
	calls := 0

	inner := harness.StepFunc(func(_ context.Context, h *domain.Handoff) (*domain.Handoff, error) {
		calls++
		snapshot := make(map[string]string)
		for k, v := range h.Context {
			snapshot[k] = v
		}
		capturedContexts = append(capturedContexts, snapshot)

		if calls == 1 {
			return nil, fmt.Errorf("logic error on first call")
		}
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("recovered")}}, nil
	})

	healer := &harness.SelfHealer{Restorer: &noopRestorer{}, StepIndex: 0}
	wrapped := healer.Wrap(inner)

	plan := &domain.ExecutionPlan{Steps: []domain.Step{{Query: "test"}}}
	_, err := (&executer.DAGExecutor{}).Execute(t.Context(), plan, nil, healedDAGStep(wrapped))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(capturedContexts) < 2 {
		t.Fatalf("expected at least 2 calls, got %d", calls)
	}
	retryCtx := capturedContexts[1]
	if retryCtx["_heal_error"] == "" {
		t.Error("expected _heal_error in retry handoff context, got empty")
	}
	if retryCtx["_heal_attempt"] != "1" {
		t.Errorf("expected _heal_attempt=1, got %q", retryCtx["_heal_attempt"])
	}
}

// ── Cycle 4: HealingExhaustedError cancels sibling parallel steps ─────────────

func TestHealedStep_ExhaustedError_CancelsSiblings(t *testing.T) {
	step0calls := 0
	step0Inner := harness.StepFunc(func(_ context.Context, _ *domain.Handoff) (*domain.Handoff, error) {
		step0calls++
		return nil, fmt.Errorf("fail-%d", step0calls)
	})
	healer0 := &harness.SelfHealer{Restorer: &noopRestorer{}, StepIndex: 0}
	wrapped0 := healer0.Wrap(step0Inner)

	siblingCancelled := make(chan struct{})
	step1Fn := func(ctx context.Context, _ *domain.Handoff) (*domain.Handoff, error) {
		<-ctx.Done()
		close(siblingCancelled)
		return nil, ctx.Err()
	}

	dagStepFn := executer.StepFunc(func(ctx context.Context, i int, h *domain.Handoff) (*domain.Handoff, error) {
		if i == 0 {
			return wrapped0(ctx, h)
		}
		return step1Fn(ctx, h)
	})

	plan := &domain.ExecutionPlan{
		Steps: []domain.Step{
			{Query: "exhaust"},  // step 0 — no deps
			{Query: "sibling"}, // step 1 — no deps (parallel)
		},
	}

	_, err := (&executer.DAGExecutor{}).Execute(t.Context(), plan, nil, dagStepFn)
	if err == nil {
		t.Fatal("expected error from exhausted healing")
	}
	var healErr *harness.HealingExhaustedError
	if !errors.As(err, &healErr) {
		t.Errorf("expected HealingExhaustedError, got %T: %v", err, err)
	}

	select {
	case <-siblingCancelled:
		// good — sibling was cancelled by DAGExecutor cancel-on-first-error
	case <-time.After(2 * time.Second):
		t.Error("sibling step was not cancelled within 2s")
	}
}
