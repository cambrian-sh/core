package harness

import (
	"sync"

	"github.com/cambrian-sh/core/domain"
)

// CapturingTelemetryObserver records all TelemetryObserver calls for test assertions.
type CapturingTelemetryObserver struct {
	mu                      sync.Mutex
	TaskCompletedEvents     []domain.TaskEvent
	EvictedAgentIDs         []string
	ConwipWaits             []int64
	AuctionNoWinners        []string
	SchemaMismatches        []struct{ AgentID, Kind string }
	PlanCompletedEvents     []domain.PlanEvent
	RetrievalSessions       []domain.RetrievalSession
	ContradictionResolutions []domain.ContradictionResolution
}

func (o *CapturingTelemetryObserver) OnTaskCompleted(evt domain.TaskEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.TaskCompletedEvents = append(o.TaskCompletedEvents, evt)
}

func (o *CapturingTelemetryObserver) OnSessionEvicted(agentID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.EvictedAgentIDs = append(o.EvictedAgentIDs, agentID)
}

func (o *CapturingTelemetryObserver) OnConwipWait(durationMs int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ConwipWaits = append(o.ConwipWaits, durationMs)
}

func (o *CapturingTelemetryObserver) OnAuctionNoWinner(taskID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.AuctionNoWinners = append(o.AuctionNoWinners, taskID)
}

func (o *CapturingTelemetryObserver) OnSchemaMismatch(agentID, kind string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.SchemaMismatches = append(o.SchemaMismatches, struct{ AgentID, Kind string }{agentID, kind})
}

func (o *CapturingTelemetryObserver) OnPlanCompleted(evt domain.PlanEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.PlanCompletedEvents = append(o.PlanCompletedEvents, evt)
}

func (o *CapturingTelemetryObserver) OnRetrievalCompleted(session domain.RetrievalSession) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.RetrievalSessions = append(o.RetrievalSessions, session)
}

func (o *CapturingTelemetryObserver) OnContradictionResolved(res domain.ContradictionResolution) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ContradictionResolutions = append(o.ContradictionResolutions, res)
}
