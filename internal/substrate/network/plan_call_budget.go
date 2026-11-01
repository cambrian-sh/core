package network

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/cambrian-sh/core/domain"
)

// ErrPlannerCallBudget is returned when one plan has used up its planner calls.
var ErrPlannerCallBudget = errors.New("planner call budget for this plan is exhausted")

// planCallBudget is ONE per-plan budget of planner calls, shared by the two
// places a plan re-asks the planner: planWithValidation's retry after an invalid
// DAG, and the DAGExecutor's replan handler
// (T-20261101-planner-validation-retries-and-replans-compound).
//
// They used to be counted separately — one validation retry, then up to
// execution.plan.max_replan_attempts replans — so the knob an operator reads as
// "how many times may the planner be re-asked" did not bound it, and the two
// multiplied. Now execution.plan.max_replan_attempts is exactly that: the plan
// gets 1 + max_replan_attempts planner calls in total, whatever mix of
// validation retries and replans spends them. 0 therefore also disables the
// validation retry, consistent with "0 disables replan".
//
// Only the planning calls draw on it. Thought steps and the checkpoint
// validator also use the planner's generator, but they are plan EXECUTION, not
// re-planning, and stay outside — they are metered by the ADR-0131 spend
// ledger like every other LLM call.
type planCallBudget struct {
	remaining atomic.Int64
	limit     int
}

func newPlanCallBudget(maxReplanAttempts int) *planCallBudget {
	if maxReplanAttempts < 0 {
		maxReplanAttempts = 0
	}
	b := &planCallBudget{limit: 1 + maxReplanAttempts}
	b.remaining.Store(int64(b.limit))
	return b
}

// take spends one planner call, or reports that the plan has none left.
func (b *planCallBudget) take() error {
	if b == nil {
		return nil
	}
	if b.remaining.Add(-1) < 0 {
		return fmt.Errorf("%w (%d calls: 1 + execution.plan.max_replan_attempts)", ErrPlannerCallBudget, b.limit)
	}
	return nil
}

// planner wraps p so GetExecutionPlan draws on the budget. Generate passes
// through untouched (see the type comment).
func (b *planCallBudget) planner(p Planner) Planner {
	return &budgetedPlanner{inner: p, budget: b}
}

// generator wraps p's Generate for the replan handler, which re-plans through
// Generate rather than GetExecutionPlan.
func (b *planCallBudget) generator(p Planner) domain.Generator {
	return &budgetedReplanGenerator{inner: p, budget: b}
}

type budgetedPlanner struct {
	inner  Planner
	budget *planCallBudget
}

func (p *budgetedPlanner) GetExecutionPlan(ctx context.Context, userInput string) (*domain.ExecutionPlan, error) {
	if err := p.budget.take(); err != nil {
		return nil, err
	}
	return p.inner.GetExecutionPlan(ctx, userInput)
}

func (p *budgetedPlanner) Generate(ctx context.Context, prompt string) (string, error) {
	return p.inner.Generate(ctx, prompt)
}

type budgetedReplanGenerator struct {
	inner  Planner
	budget *planCallBudget
}

func (g *budgetedReplanGenerator) Generate(ctx context.Context, prompt string) (string, error) {
	if err := g.budget.take(); err != nil {
		return "", err
	}
	return g.inner.Generate(ctx, prompt)
}
