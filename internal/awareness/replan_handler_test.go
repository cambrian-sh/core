package awareness

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
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
	if strings.Contains(gen.lastPrompt, "BUDGET CONSTRAINT") {
		t.Errorf("expected prompt NOT to contain 'BUDGET CONSTRAINT' for plain error")
	}
}

// Cycle 6: BudgetExceededError path is unaffected — prompt contains "BUDGET CONSTRAINT" not "CHECKPOINT FAILURE"
func TestReplan_BudgetExceededError_UnaffectedByCheckpointBranch(t *testing.T) {
	gen := &capturingGenerator{}
	h := makeHandler(gen)
	plan := makePlan(domain.Step{Query: "do something"})
	err := &domain.BudgetExceededError{RunningCost: 5.0, MaxCost: 3.0}

	_, replanErr := h.Replan(context.Background(), 0, err, nil, plan)
	if replanErr != nil {
		t.Fatalf("unexpected error: %v", replanErr)
	}
	if !strings.Contains(gen.lastPrompt, "BUDGET CONSTRAINT") {
		t.Errorf("expected prompt to contain 'BUDGET CONSTRAINT', got:\n%s", gen.lastPrompt)
	}
	if strings.Contains(gen.lastPrompt, "CHECKPOINT FAILURE") {
		t.Errorf("expected prompt NOT to contain 'CHECKPOINT FAILURE' for BudgetExceededError, got:\n%s", gen.lastPrompt)
	}
}

// Cycle 5a: Empty CheckpointQuery — constraint renders without panicking, uses empty quoted string
func TestReplan_SemanticCheckpointError_EmptyCheckpointQuery(t *testing.T) {
	gen := &capturingGenerator{}
	h := makeHandler(gen)
	plan := makePlan(domain.Step{Query: "do something"}) // CheckpointQuery is empty
	err := &domain.SemanticCheckpointError{StepIndex: 0, Assessment: "not coherent", OriginalResult: "bad"}

	_, replanErr := h.Replan(context.Background(), 0, err, nil, plan)
	if replanErr != nil {
		t.Fatalf("unexpected error: %v", replanErr)
	}
	// Should contain the quoted empty string
	if !strings.Contains(gen.lastPrompt, `""`) {
		t.Errorf("expected prompt to contain empty quoted string, got:\n%s", gen.lastPrompt)
	}
}

// Cycle 5b: failedStep out of range — no panic, uses empty string for quoted question
func TestReplan_SemanticCheckpointError_OutOfRangeStep_NoPanic(t *testing.T) {
	gen := &capturingGenerator{}
	h := makeHandler(gen)
	plan := makePlan(domain.Step{Query: "only step"}) // only 1 step, index 0
	err := &domain.SemanticCheckpointError{StepIndex: 99, Assessment: "incoherent", OriginalResult: "bad"}

	// Must not panic
	_, replanErr := h.Replan(context.Background(), 99, err, nil, plan)
	if replanErr != nil {
		t.Fatalf("unexpected error: %v", replanErr)
	}
	if !strings.Contains(gen.lastPrompt, "CHECKPOINT FAILURE") {
		t.Errorf("expected prompt to contain 'CHECKPOINT FAILURE', got:\n%s", gen.lastPrompt)
	}
	// The quoted field should be empty string
	if !strings.Contains(gen.lastPrompt, `""`) {
		t.Errorf("expected prompt to contain empty quoted string for out-of-range step, got:\n%s", gen.lastPrompt)
	}
}

// Cycle 4: When the failed step has CheckpointQuery set, it appears quoted in the constraint
func TestReplan_SemanticCheckpointError_ContainsCheckpointQuery(t *testing.T) {
	gen := &capturingGenerator{}
	h := makeHandler(gen)
	cpQuery := "Is the answer factually correct and complete?"
	plan := makePlan(domain.Step{
		Query:           "do something",
		CheckpointAfter: true,
		CheckpointQuery: cpQuery,
	})
	err := &domain.SemanticCheckpointError{StepIndex: 0, Assessment: "not coherent", OriginalResult: "bad"}

	_, replanErr := h.Replan(context.Background(), 0, err, nil, plan)
	if replanErr != nil {
		t.Fatalf("unexpected error: %v", replanErr)
	}
	quoted := `"` + cpQuery + `"`
	if !strings.Contains(gen.lastPrompt, quoted) {
		t.Errorf("expected prompt to contain quoted CheckpointQuery %s, got:\n%s", quoted, gen.lastPrompt)
	}
}

// Cycle 3: CHECKPOINT FAILURE block contains the Assessment string
func TestReplan_SemanticCheckpointError_ContainsAssessment(t *testing.T) {
	gen := &capturingGenerator{}
	h := makeHandler(gen)
	plan := makePlan(domain.Step{Query: "do something"})
	assessment := "the output contradicts the user's intent"
	err := &domain.SemanticCheckpointError{StepIndex: 0, Assessment: assessment, OriginalResult: "blah"}

	_, replanErr := h.Replan(context.Background(), 0, err, nil, plan)
	if replanErr != nil {
		t.Fatalf("unexpected error: %v", replanErr)
	}
	if !strings.Contains(gen.lastPrompt, assessment) {
		t.Errorf("expected prompt to contain assessment %q, got:\n%s", assessment, gen.lastPrompt)
	}
}

// Cycle 2: CHECKPOINT FAILURE block contains the correct StepIndex
func TestReplan_SemanticCheckpointError_ContainsStepIndex(t *testing.T) {
	gen := &capturingGenerator{}
	h := makeHandler(gen)
	plan := makePlan(
		domain.Step{Query: "step zero"},
		domain.Step{Query: "step one"},
		domain.Step{Query: "step two"},
	)
	err := &domain.SemanticCheckpointError{StepIndex: 2, Assessment: "too vague", OriginalResult: "result"}

	_, replanErr := h.Replan(context.Background(), 2, err, nil, plan)
	if replanErr != nil {
		t.Fatalf("unexpected error: %v", replanErr)
	}
	if !strings.Contains(gen.lastPrompt, "Step 2") {
		t.Errorf("expected prompt to contain 'Step 2', got:\n%s", gen.lastPrompt)
	}
}

// Cycle 1: SemanticCheckpointError → prompt contains "CHECKPOINT FAILURE"
func TestReplan_SemanticCheckpointError_ContainsCheckpointFailure(t *testing.T) {
	gen := &capturingGenerator{}
	h := makeHandler(gen)
	plan := makePlan(domain.Step{Query: "do something"})
	err := &domain.SemanticCheckpointError{StepIndex: 0, Assessment: "output was vague", OriginalResult: "blah"}

	_, replanErr := h.Replan(context.Background(), 0, err, nil, plan)
	if replanErr != nil {
		t.Fatalf("unexpected error: %v", replanErr)
	}
	if !strings.Contains(gen.lastPrompt, "CHECKPOINT FAILURE") {
		t.Errorf("expected prompt to contain 'CHECKPOINT FAILURE', got:\n%s", gen.lastPrompt)
	}
}
