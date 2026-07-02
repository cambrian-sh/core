package domain

import "fmt"

// BudgetExceededError is returned by the DAGExecutor when the cumulative cost
// of completed steps surpasses the configured MaxPlanCost ceiling.
type BudgetExceededError struct {
	RunningCost float64
	MaxCost     float64
}

func (e *BudgetExceededError) Error() string {
	return fmt.Sprintf("plan budget exceeded: running cost %.4f > max %.4f", e.RunningCost, e.MaxCost)
}

// SemanticCheckpointError is returned by the DAGExecutor when a step's output
// fails the coherence gate before downstream steps are dispatched.
type SemanticCheckpointError struct {
	StepIndex      int
	Assessment     string // full Thought step output, contains "REPLAN_SIGNAL"
	OriginalResult string // step output that failed the coherence check
}

func (e *SemanticCheckpointError) Error() string {
	return fmt.Sprintf("semantic checkpoint failed at step %d: %s", e.StepIndex, e.Assessment)
}
