package domain

// TelemetryObserver is the injection point for all telemetry signals emitted
// by core packages. Implementations must be safe for concurrent use.
// A nil value must never be used — assign NoopTelemetryObserver{} instead.
type TelemetryObserver interface {
	OnTaskCompleted(evt TaskEvent)
	OnSessionEvicted(agentID string)
	OnConwipWait(durationMs int64)
	OnAuctionNoWinner(taskID string)
	OnSchemaMismatch(agentID, kind string)
	OnPlanCompleted(event PlanEvent)
	OnRetrievalCompleted(session RetrievalSession)
	OnContradictionResolved(resolution ContradictionResolution)
}

// NoopTelemetryObserver satisfies TelemetryObserver with empty methods.
// Wire this when telemetry is unconfigured or in tests that do not assert metrics.
type NoopTelemetryObserver struct{}

func (NoopTelemetryObserver) OnTaskCompleted(_ TaskEvent)                  {}
func (NoopTelemetryObserver) OnSessionEvicted(_ string)                    {}
func (NoopTelemetryObserver) OnConwipWait(_ int64)                         {}
func (NoopTelemetryObserver) OnAuctionNoWinner(_ string)                  {}
func (NoopTelemetryObserver) OnSchemaMismatch(_, _ string)               {}
func (NoopTelemetryObserver) OnPlanCompleted(_ PlanEvent)                 {}
func (NoopTelemetryObserver) OnRetrievalCompleted(_ RetrievalSession)       {}
func (NoopTelemetryObserver) OnContradictionResolved(_ ContradictionResolution) {}

var _ TelemetryObserver = NoopTelemetryObserver{}

