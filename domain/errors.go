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
