package awareness

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// capturingGenerator captures the last prompt passed to Generate and returns
// a minimal valid plan JSON.
type capturingGenerator struct{ lastPrompt string }

func (g *capturingGenerator) Generate(_ context.Context, prompt string) (string, error) {
	g.lastPrompt = prompt
	return `{"steps":[{"query":"fix it"}],"subject":"Replan: test"}`, nil
}

func makeHandler(gen *capturingGenerator) *PlannerReplanHandler {
	return NewPlannerReplanHandler(gen)
}

func makePlan(steps ...domain.Step) *domain.ExecutionPlan {
	return &domain.ExecutionPlan{Subject: "test", Steps: steps}
}

// Cycle 7: Plain error — neither constraint block is injected
func TestReplan_PlainError_NoConstraintBlock(t *testing.T) {
	t.Parallel()
	gen := &capturingGenerator{}
	h := makeHandler(gen)
	plan := makePlan(domain.Step{Query: "do something"})
	err := fmt.Errorf("some generic failure")

	_, replanErr := h.Replan(context.Background(), 0, err, nil, plan)
	if replanErr != nil {
		t.Fatalf("unexpected error: %v", replanErr)
	}
	if strings.Contains(gen.lastPrompt, "CHECKPOINT FAILURE") {
		t.Errorf("expected prompt NOT to contain 'CHECKPOINT FAILURE' for plain error")
	}
	if strings.Contains(gen.lastPrompt, "</partial_results>\n<budget_exceeded/>") {
		t.Errorf("a plain error must not carry the budget marker")
	}
}

// BudgetExceededError puts the <budget_exceeded/> marker in the message. (The CHECKPOINT FAILURE block was
// removed with the checkpoint gate, ADR-0139 D10; the negative check stays as a guard.)
func TestReplan_BudgetExceededError_InjectsBudgetConstraint(t *testing.T) {
	t.Parallel()
	gen := &capturingGenerator{}
	h := makeHandler(gen)
	plan := makePlan(domain.Step{Query: "do something"})
	err := &domain.BudgetExceededError{RunningCost: 5.0, MaxCost: 3.0}

	_, replanErr := h.Replan(context.Background(), 0, err, nil, plan)
	if replanErr != nil {
		t.Fatalf("unexpected error: %v", replanErr)
	}
	if !strings.Contains(gen.lastPrompt, "</partial_results>\n<budget_exceeded/>\n<failure>") {
		t.Errorf("expected the budget marker before <failure>, got:\n%s", gen.lastPrompt)
	}
	if strings.Contains(gen.lastPrompt, "CHECKPOINT FAILURE") {
		t.Errorf("expected prompt NOT to contain 'CHECKPOINT FAILURE' for BudgetExceededError, got:\n%s", gen.lastPrompt)
	}
}
