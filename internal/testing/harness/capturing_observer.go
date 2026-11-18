package harness

import (
	"context"

	"sync"

	"github.com/cambrian-sh/core/domain"
)

// CapturingTelemetryObserver records all TelemetryObserver calls for test assertions.
type CapturingTelemetryObserver struct {
	mu                       sync.Mutex
	TaskCompletedEvents      []domain.TaskEvent
	EvictedAgentIDs          []string
	ConwipWaits              []int64
	SchemaMismatches         []struct{ AgentID, Kind string }
	PlanCompletedEvents      []domain.PlanEvent
	ContradictionResolutions []domain.ContradictionResolution
}

func (o *CapturingTelemetryObserver) OnTaskCompleted(_ context.Context, evt domain.TaskEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.TaskCompletedEvents = append(o.TaskCompletedEvents, evt)
}

func (o *CapturingTelemetryObserver) OnSessionEvicted(_ context.Context, agentID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.EvictedAgentIDs = append(o.EvictedAgentIDs, agentID)
}

func (o *CapturingTelemetryObserver) OnConwipWait(_ context.Context, durationMs int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ConwipWaits = append(o.ConwipWaits, durationMs)
}

func (o *CapturingTelemetryObserver) OnSchemaMismatch(_ context.Context, agentID, kind string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.SchemaMismatches = append(o.SchemaMismatches, struct{ AgentID, Kind string }{agentID, kind})
}

func (o *CapturingTelemetryObserver) OnPlanCompleted(_ context.Context, evt domain.PlanEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.PlanCompletedEvents = append(o.PlanCompletedEvents, evt)
}

func (o *CapturingTelemetryObserver) OnContradictionResolved(_ context.Context, res domain.ContradictionResolution) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.ContradictionResolutions = append(o.ContradictionResolutions, res)
}
