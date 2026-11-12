package yield

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// T-20261112-self-healer-retries-a-step-whose-yield-chain: the yield limit errors
// are deterministic, and the sub-goal budget lives on the step, so a second
// attempt of the same step (the self-healer re-runs Execute on the step's ctx)
// does not get a fresh budget.

func TestYieldLimitErrorsAreDeterministic(t *testing.T) {
	for _, err := range []error{ErrMaxDepth, ErrMaxSubgoals, ErrRepeatedIntent, ErrInvalidSubgoal, ErrUnknownYielder} {
		wrapped := errors.Join(errors.New("context"), err)
		if got := domain.ClassOfStepError(wrapped); got != domain.StepFailureDeterministic {
			t.Fatalf("%v: class %q, want deterministic", err, got)
		}
		if !errors.Is(wrapped, err) {
			t.Fatalf("%v: no longer comparable with errors.Is", err)
		}
	}
}

func TestStepBudget_PersistsAcrossAttemptsOfTheSameStep(t *testing.T) {
	// The step's agent yields a sub-goal every time it is dispatched; each
	// sub-goal is answered and the agent answers once resumed.
	f := &fakeDispatcher{
		route: map[string]string{"analyse the 10-K": "parent", "*": "helper"},
		agents: map[string]agentFn{
			"parent": delegatingAgent("summarise the risk factors", "", ""),
			"helper": func(*domain.Handoff) *domain.Handoff { return answer("risk summary") },
		},
	}
	r := NewResolver(f, Limits{MaxDepth: 3, MaxSubgoals: 2})
	stepCtx := WithStepBudget(context.Background())

	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := r.Execute(stepCtx, stepTask(), stepHandoff()); err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
	}
	if got := StepBudgetFrom(stepCtx).Used(); got != 2 {
		t.Fatalf("step budget used = %d, want 2 (one per attempt, on the same budget)", got)
	}
	// The third attempt of the same step finds the budget spent: a deterministic
	// failure, not a fresh allowance.
	_, err := r.Execute(stepCtx, stepTask(), stepHandoff())
	if !errors.Is(err, ErrMaxSubgoals) {
		t.Fatalf("third attempt: err = %v, want ErrMaxSubgoals", err)
	}
	if domain.ClassOfStepError(err) != domain.StepFailureDeterministic {
		t.Fatalf("class = %q", domain.ClassOfStepError(err))
	}

	// A different step (a fresh step ctx) has its own budget.
	if _, err := r.Execute(WithStepBudget(context.Background()), stepTask(), stepHandoff()); err != nil {
		t.Fatalf("a new step must get its own budget: %v", err)
	}
}

func TestSubgoalTaskIsMarked(t *testing.T) {
	f := &fakeDispatcher{
		route: map[string]string{"analyse the 10-K": "parent", "*": "helper"},
		agents: map[string]agentFn{
			"parent": delegatingAgent("run a PowerShell command", "PowerShell command execution", ""),
			"helper": func(*domain.Handoff) *domain.Handoff { return answer("done") },
		},
	}
	r := NewResolver(f, defaultLimits)
	if _, err := r.Execute(WithStepBudget(context.Background()), stepTask(), stepHandoff()); err != nil {
		t.Fatal(err)
	}
	if len(f.tasks) != 2 || !f.tasks[1].Subgoal || f.tasks[0].Subgoal {
		t.Fatalf("tasks %+v: only the sub-goal is marked Subgoal", f.tasks)
	}
}

func TestSubgoalCapabilityMissing_FailsTheStepForReplan(t *testing.T) {
	// The binder refuses the sub-goal because the step's composition lacks the
	// capability (compose.Composer.subgoalGap): the step fails with the typed
	// error in its chain, class needs_replan — the parent is not resumed.
	gap := &domain.CapabilityMissingError{TaskID: "task-0/yield-1", Needed: []string{"PowerShell command execution"},
		Intent: "run a PowerShell command", Source: domain.CapabilitySourceSubgoal}
	f := &fakeDispatcher{
		route:   map[string]string{"analyse the 10-K": "parent"},
		execErr: map[string]error{"run a PowerShell command": gap},
		agents: map[string]agentFn{
			"parent": delegatingAgent("run a PowerShell command", "PowerShell command execution", ""),
		},
	}
	r := NewResolver(f, defaultLimits)
	_, err := r.Execute(WithStepBudget(context.Background()), stepTask(), stepHandoff())
	var cm *domain.CapabilityMissingError
	if !errors.As(err, &cm) {
		t.Fatalf("err = %v, want a CapabilityMissingError in the chain", err)
	}
	if domain.ClassOfStepError(err) != domain.StepFailureNeedsReplan {
		t.Fatalf("class = %q, want needs_replan", domain.ClassOfStepError(err))
	}
	if len(f.calls) != 0 {
		t.Fatalf("the yielding agent was resumed %v; the step must end", f.calls)
	}
}
