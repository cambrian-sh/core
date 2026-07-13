package executer

import (
	"context"

	"github.com/cambrian-sh/core/domain"
)

// ReplanHandler is called by DAGExecutor when a step exhausts all
// intra-step retries and inter-step fallback candidates.
type ReplanHandler interface {
	Replan(
		ctx context.Context,
		failedStep int,
		err error,
		partialContext map[string]string,
		originalPlan *domain.ExecutionPlan,
	) (*domain.ExecutionPlan, error)
}
