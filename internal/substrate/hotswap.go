package substrate

import "github.com/cambrian-sh/cambrian-runtime/domain"

// HotSwapPlan constructs a merged ExecutionPlan for Ctrl+I replanning.
// It preserves the first `completedCount` steps from the original plan
// unchanged, then appends the new steps with their DependsOn indices
// remapped by +completedCount.
//
// Root steps in the new plan (DependsOn == nil or empty) that are also leaf
// steps within the sub-plan (no other new step depends on them) are
// automatically wired to depend on the last completed step so the merged DAG
// remains connected. Root steps that have within-sub-plan successors are left
// as true roots — their successors will chain back to the completed portion
// through the sub-plan's own dependency structure.
func HotSwapPlan(original []domain.Step, completedCount int, newSteps []domain.Step) *domain.ExecutionPlan {
	offset := completedCount
	merged := make([]domain.Step, 0, offset+len(newSteps))

	// Preserve completed steps verbatim.
	for i := 0; i < offset && i < len(original); i++ {
		merged = append(merged, original[i])
	}

	// Pass 1: determine which new-step indices are referenced by other new steps.
	// A new step at index i is a "depended-upon" step if any other new step lists i
	// in its DependsOn. Such steps should not inherit lastCompleted automatically.
	dependedUpon := make(map[int]bool, len(newSteps))
	for _, s := range newSteps {
		for _, dep := range s.DependsOn {
			dependedUpon[dep] = true
		}
	}

	// Pass 2: build merged steps with remapped DependsOn.
	lastCompleted := offset - 1
	for i, s := range newSteps {
		remapped := make([]int, len(s.DependsOn))
		for j, dep := range s.DependsOn {
			remapped[j] = dep + offset
		}
		// A root step (no deps) that is NOT depended upon by other new steps
		// inherits a dependency on the last completed step so the merged DAG
		// remains connected.
		if len(remapped) == 0 && lastCompleted >= 0 && !dependedUpon[i] {
			remapped = []int{lastCompleted}
		}
		merged = append(merged, domain.Step{
			Query:     s.Query,
			DependsOn: remapped,
			IsThought: s.IsThought,
		})
	}

	return &domain.ExecutionPlan{Steps: merged}
}
