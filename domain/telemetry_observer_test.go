package domain_test

import (
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

var _ domain.TelemetryObserver = domain.NoopTelemetryObserver{}

func TestNoopTelemetryObserver_AllMethodsNoPanic(t *testing.T) {
	obs := domain.NoopTelemetryObserver{}

	obs.OnTaskCompleted(domain.TaskEvent{
		TaskID:        "task-1",
		AgentID:       "agent-1",
		BudgetOverrun: true,
	})

	obs.OnSessionEvicted("agent-1")

	obs.OnConwipWait(150)

	obs.OnAuctionNoWinner("task-1")

	obs.OnSchemaMismatch("agent-1", "missing-field")

	obs.OnPlanCompleted(domain.PlanEvent{
		PlanID:  "plan-1",
		Outcome: domain.PlanOutcomeSuccess,
	})

	obs.OnRetrievalCompleted(domain.RetrievalSession{
		SessionID: "ret-1",
		Query:     "test query",
	})

	obs.OnContradictionResolved(domain.ContradictionResolution{
		ResolutionID: "cr-1",
		DocAID:       "doc-a",
		DocBID:       "doc-b",
	})
}
