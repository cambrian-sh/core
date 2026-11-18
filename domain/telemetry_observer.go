package domain

import "context"

// TelemetryObserver is the injection point for all telemetry signals emitted
// by core packages. Implementations must be safe for concurrent use.
// A nil value must never be used — assign NoopTelemetryObserver{} instead.
//
// Every callback takes the caller's ctx first (AGENTS.md §2.7): an
// implementation that records metrics or spans records them under the request's
// trace instead of a detached Background context
// (T-20261118-remaining-context-background-leaks-on-request).
type TelemetryObserver interface {
	OnTaskCompleted(ctx context.Context, evt TaskEvent)
	OnSessionEvicted(ctx context.Context, agentID string)
	OnConwipWait(ctx context.Context, durationMs int64)
	OnSchemaMismatch(ctx context.Context, agentID, kind string)
	OnPlanCompleted(ctx context.Context, event PlanEvent)
	OnContradictionResolved(ctx context.Context, resolution ContradictionResolution)
}

// NoopTelemetryObserver satisfies TelemetryObserver with empty methods.
// Wire this when telemetry is unconfigured or in tests that do not assert metrics.
type NoopTelemetryObserver struct{}

func (NoopTelemetryObserver) OnTaskCompleted(context.Context, TaskEvent)                       {}
func (NoopTelemetryObserver) OnSessionEvicted(context.Context, string)                         {}
func (NoopTelemetryObserver) OnConwipWait(context.Context, int64)                              {}
func (NoopTelemetryObserver) OnSchemaMismatch(context.Context, string, string)                 {}
func (NoopTelemetryObserver) OnPlanCompleted(context.Context, PlanEvent)                       {}
func (NoopTelemetryObserver) OnContradictionResolved(context.Context, ContradictionResolution) {}

var _ TelemetryObserver = NoopTelemetryObserver{}
