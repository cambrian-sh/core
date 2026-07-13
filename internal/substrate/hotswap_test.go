package substrate_test

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
	"github.com/cambrian-sh/core/internal/substrate"
)

func TestHotSwapPlan_PreservesCompletedSteps(t *testing.T) {
	original := []domain.Step{
		{Query: "step-0", DependsOn: []int{}},
		{Query: "step-1", DependsOn: []int{0}},
		{Query: "step-2", DependsOn: []int{1}},
	}
	newSteps := []domain.Step{
		{Query: "new-0", DependsOn: []int{}},
		{Query: "new-1", DependsOn: []int{0}},
	}

	result := substrate.HotSwapPlan(original, 2, newSteps)

	if len(result.Steps) != 4 {
		t.Fatalf("expected 4 steps (2 preserved + 2 new), got %d", len(result.Steps))
	}
	if result.Steps[0].Query != "step-0" {
		t.Errorf("expected preserved step-0, got %q", result.Steps[0].Query)
	}
	if result.Steps[1].Query != "step-1" {
		t.Errorf("expected preserved step-1, got %q", result.Steps[1].Query)
	}
}

func TestHotSwapPlan_RemapsNewStepDependencies(t *testing.T) {
	original := []domain.Step{
		{Query: "old-0", DependsOn: []int{}},
		{Query: "old-1", DependsOn: []int{0}},
	}
	newSteps := []domain.Step{
		{Query: "new-0", DependsOn: []int{}},  // no deps — root
		{Query: "new-1", DependsOn: []int{0}}, // depends on new-0
	}

	result := substrate.HotSwapPlan(original, 2, newSteps)

	// new-0 is at index 2; its DependsOn should be empty (root in new plan).
	if len(result.Steps[2].DependsOn) != 0 {
		t.Errorf("new-0 should have no DependsOn, got %v", result.Steps[2].DependsOn)
	}
	// new-1 is at index 3; depends on new-0 (index 2 in merged plan).
	if len(result.Steps[3].DependsOn) != 1 || result.Steps[3].DependsOn[0] != 2 {
		t.Errorf("new-1 should depend on index 2, got %v", result.Steps[3].DependsOn)
	}
}

func TestHotSwapPlan_ZeroCompleted(t *testing.T) {
	original := []domain.Step{
		{Query: "old-0", DependsOn: []int{}},
	}
	newSteps := []domain.Step{
		{Query: "new-0", DependsOn: []int{}},
	}

	result := substrate.HotSwapPlan(original, 0, newSteps)

	if len(result.Steps) != 1 {
		t.Fatalf("expected 1 step (0 preserved + 1 new), got %d", len(result.Steps))
	}
	if result.Steps[0].Query != "new-0" {
		t.Errorf("expected new-0, got %q", result.Steps[0].Query)
	}
}

func TestHotSwapPlan_AllCompleted_EmptyNew(t *testing.T) {
	original := []domain.Step{
		{Query: "step-0", DependsOn: []int{}},
	}

	result := substrate.HotSwapPlan(original, 1, nil)

	if len(result.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(result.Steps))
	}
}

func TestHotSwapPlan_NewStepInheritsLastCompleted(t *testing.T) {
	// If new root step has no deps, it should depend on the last completed step
	// so the merged DAG remains connected.
	original := []domain.Step{
		{Query: "old-0", DependsOn: []int{}},
		{Query: "old-1", DependsOn: []int{0}},
	}
	newSteps := []domain.Step{
		{Query: "continuation", DependsOn: []int{}}, // root in sub-plan
	}

	result := substrate.HotSwapPlan(original, 2, newSteps)

	// "continuation" is index 2; its DependsOn must include 1 (last completed)
	// so the DAG knows not to run it until old-1 finishes.
	found := false
	for _, dep := range result.Steps[2].DependsOn {
		if dep == 1 {
			found = true
		}
	}
	if !found {
		t.Errorf("expected continuation to depend on last-completed index 1, got %v", result.Steps[2].DependsOn)
	}
}

// --- Cycle 4: DAGExecutor CompletedIndices ---

func TestDAGExecutor_TracksCompletedIndices(t *testing.T) {
	dag := &executer.DAGExecutor{}
	plan := &domain.ExecutionPlan{
		Steps: []domain.Step{
			{Query: "step-0", DependsOn: []int{}},
			{Query: "step-1", DependsOn: []int{0}},
		},
	}

	stepFn := func(ctx context.Context, idx int, h *domain.Handoff) (*domain.Handoff, error) {
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("ok")}}, nil
	}

	_, err := dag.Execute(context.Background(), plan, nil, stepFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	completed := dag.CompletedIndices()
	if len(completed) != 2 {
		t.Errorf("expected 2 completed indices, got %d: %v", len(completed), completed)
	}
}
