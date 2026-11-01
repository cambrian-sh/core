package network

import (
	"context"
	"errors"
	"testing"
)

// T-20261101-planner-validation-retries-and-replans-compound: validation retries
// and replans draw on ONE budget of 1 + max_replan_attempts planner calls.
func TestPlanCallBudget_ValidationRetryAndReplansShareOneCount(t *testing.T) {
	t.Parallel()
	mock := &mockPlanner{
		responses: []plannerResponse{{plan: cyclicPlan()}, {plan: cyclicPlan()}},
		genResp:   `{"subject":"replan","steps":[]}`,
	}
	b := newPlanCallBudget(2) // 3 planner calls in total

	if _, err := planWithValidation(context.Background(), b.planner(mock), "req"); err == nil {
		t.Fatal("a plan that never validates must fail")
	}
	if len(mock.calls) != 2 {
		t.Fatalf("initial call + one validation retry expected, got %d", len(mock.calls))
	}
	replan := b.generator(mock)
	if _, err := replan.Generate(context.Background(), "replan 1"); err != nil {
		t.Fatalf("third call is within budget: %v", err)
	}
	if _, err := replan.Generate(context.Background(), "replan 2"); !errors.Is(err, ErrPlannerCallBudget) {
		t.Fatalf("fourth planner call must be refused, got %v", err)
	}
	if total := len(mock.calls) + len(mock.genCalls); total != 3 {
		t.Fatalf("planner was called %d times, budget is 3", total)
	}
}

// max_replan_attempts = 0 disables the validation retry too: one planner call.
func TestPlanCallBudget_ZeroAttemptsMeansOnePlannerCall(t *testing.T) {
	t.Parallel()
	mock := &mockPlanner{responses: []plannerResponse{{plan: cyclicPlan()}}}
	_, err := planWithValidation(context.Background(), newPlanCallBudget(0).planner(mock), "req")
	if !errors.Is(err, ErrPlannerCallBudget) {
		t.Fatalf("want ErrPlannerCallBudget on the would-be retry, got %v", err)
	}
	if len(mock.calls) != 1 {
		t.Fatalf("exactly one planner call expected, got %d", len(mock.calls))
	}
}

// Thought steps and checkpoints use the planner's Generate through the plain
// planner, not the budgeted replan generator — the budgeted planner's Generate
// passes through untouched.
func TestPlanCallBudget_PlannerGenerateIsNotCharged(t *testing.T) {
	t.Parallel()
	mock := &mockPlanner{genResp: "ok"}
	p := newPlanCallBudget(0).planner(mock)
	for i := 0; i < 3; i++ {
		if _, err := p.Generate(context.Background(), "thought"); err != nil {
			t.Fatalf("Generate charged the budget: %v", err)
		}
	}
}
