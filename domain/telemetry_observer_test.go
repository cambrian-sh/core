package domain_test

import (
	"testing"

	"github.com/cambrian-sh/core/domain"
)

var _ domain.TelemetryObserver = domain.NoopTelemetryObserver{}

func TestNoopTelemetryObserver_AllMethodsNoPanic(t *testing.T) {
	t.Parallel()
	obs := domain.NoopTelemetryObserver{}

	obs.OnTaskCompleted(t.Context(), domain.TaskEvent{
		TaskID:        "task-1",
		AgentID:       "agent-1",
		BudgetOverrun: true,
	})

	obs.OnSessionEvicted(t.Context(), "agent-1")

	obs.OnConwipWait(t.Context(), 150)

	obs.OnSchemaMismatch(t.Context(), "agent-1", "missing-field")

	obs.OnPlanCompleted(t.Context(), domain.PlanEvent{
		PlanID:  "plan-1",
		Outcome: domain.PlanOutcomeSuccess,
	})

	obs.OnContradictionResolved(t.Context(), domain.ContradictionResolution{
		ResolutionID: "cr-1",
		DocAID:       "doc-a",
		DocBID:       "doc-b",
	})
}
