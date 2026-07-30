package domain

import (
	"context"
	"time"
)

// Signal is the canonical envelope for a daemon or filesystem event signal.
// Produced by daemon agents via SignalStream or by DirectoryWatcher on file events.
// Consumed by the ReactiveEngine (ADR-0032) for condition evaluation.
type Signal struct {
	// StreamID matches the WatchConfig.Source.StreamID this signal belongs to.
	StreamID string
	// FromAgent is the agent ID or component that produced the signal.
	FromAgent string
	// Payload carries structured data from the signal source.
	// Common keys: "path", "extension", "mime_type", "price", "currency".
	Payload map[string]any
	// RawText is an optional human-readable representation of the signal.
	RawText string
	// Timestamp is when the signal was produced.
	Timestamp time.Time
}

// SignalReceiver processes incoming signals. Implementations route signals to
// the appropriate WatchConfig condition evaluator (ReactiveEngine, ADR-0032).
// The nil implementation (NoOpSignalReceiver) logs and discards. ADR-0031.
type SignalReceiver interface {
	OnSignal(ctx context.Context, signal Signal) error
}

// WatchConfigHandler handles WatchConfig CRUD operations exposed via gRPC.
// ADR-0057: promoted to domain so a downstream (premium) module can name it as the
// return type of the app.Options reactive hook. Implemented by the premium
// ReactiveEngine's WatchHandler; nil in OSS builds (RPC shells return Unimplemented).
type WatchConfigHandler interface {
	RegisterWatch(cfg WatchConfig) (string, error)
	ListWatches() ([]WatchConfig, error)
	DeleteWatch(id string) error
	SetWatchActive(id string, active bool) error
}

// WatchMetricsReader surfaces per-watch observability counters (REACT-05 / ADR-0071).
// Satisfied by the premium ReactiveEngine; nil in OSS (the RPC returns Unimplemented).
type WatchMetricsReader interface {
	WatchMetrics() []WatchMetrics
}

// Stream liveness states (contract 0074). StreamUnknown is deliberately distinct
// from StreamUnavailable: a kernel with no stream registry cannot tell, and
// reporting a guess as a fact would raise a false alarm on every watch.
const (
	StreamLive        = "live"
	StreamUnavailable = "unavailable"
	StreamUnknown     = "unknown"
)

// StreamRegistry reports whether a signal stream is alive and how many watches
// hold it (contract 0074).
//
// It closes the reactive plane's quietest failure: when the daemon feeding a
// stream crashes, every watch on that stream stops evaluating — no error, no
// dead letter, no fired event — while each watch still reads "active". Nothing
// anywhere in the system said so.
//
// Satisfied by the premium reactive engine; nil ⇒ every watch reports
// StreamUnknown, which a console must render as "cannot tell", not as healthy.
type StreamRegistry interface {
	// StreamState returns StreamLive, StreamUnavailable or StreamUnknown.
	StreamState(streamID string) string
	// StreamRefcount returns how many watches hold streamID, or -1 when the
	// registry does not count them.
	StreamRefcount(streamID string) int
}

// WatchFire is one recorded evaluation of a watch (contract 0074), backing the
// fire-history sparkline.
type WatchFire struct {
	At time.Time
	// Outcome is "fired" | "suppressed" | "failed" | "would_fire".
	//
	// "suppressed" is a rule WORKING — the condition was evaluated and said no.
	// A console rendering it as a failure trains an operator to loosen a boundary
	// that is doing its job, which is the same mistake as reading a policy denial
	// as an error.
	Outcome   string
	Error     string
	LatencyMs int64
}

// Watch fire outcomes.
const (
	FireFired      = "fired"
	FireSuppressed = "suppressed"
	FireFailed     = "failed"
	FireWouldFire  = "would_fire"
)

// WatchFireReader returns a watch's recent evaluations, newest last. Satisfied
// by the premium reactive engine; nil ⇒ no history is reported (which is NOT the
// same as "never fired").
type WatchFireReader interface {
	RecentFires(watchID string, limit int) []WatchFire
}

// ReactivePlaneBudget is the plane-wide running total against its caps
// (contract 0074).
//
// Distinct from the shed EVENT: that fires once the plane is already dropping
// work, and this is the approach to that line. Every field is -1 when not
// tracked, which is distinct from 0 — "0 plans started this hour" is a claim
// about activity, and reporting it for a counter that does not exist would make
// a busy plane look idle.
type ReactivePlaneBudget struct {
	GateEvaluationsThisHour int64
	GateEvaluationsCap      int64
	PlansStartedThisHour    int64
	PlansStartedCap         int64
	SignalsShedThisHour     int64
	WindowStarted           time.Time
}

// ReactiveBudgetReader reports the plane budget. nil ⇒ Unimplemented.
type ReactiveBudgetReader interface {
	PlaneBudget() ReactivePlaneBudget
}

// WatchBacktester replays a candidate WatchConfig over the signal journal without acting
// (REACT-05 / ADR-0071). Satisfied by the premium ReactiveEngine.
type WatchBacktester interface {
	Backtest(ctx context.Context, cfg WatchConfig, afterSeq uint64) ([]WatchBacktestVerdict, error)
}

// WatchSource identifies the origin of signals for a WatchConfig.
type WatchSource struct {
	// Type is "daemon", "filesystem", "webhook", "signal_stream", or "schedule". ADR-0072.
	Type string
	// StreamID is the identifier that arriving signals must carry to match
	// this WatchConfig. For filesystem sources, it is the watched directory path.
	StreamID string
	// Cron is the 5-field cron expression (or @-shortcut) for a "schedule" source
	// (REACT-06 / ADR-0072): the kernel emits a synthetic signal on StreamID at each
	// scheduled time, flowing through the normal condition/action pipeline.
	Cron string
	// Timezone is the IANA tz name the cron is evaluated in (e.g. "America/New_York").
	// Empty ⇒ UTC.
	Timezone string
}

// ConditionType constants for WatchConfig.ConditionType.
const (
	ConditionTypeDeterministic = "deterministic"
	ConditionTypePattern       = "pattern"
	ConditionTypeLLM           = "llm"
	// ConditionTypeAlways skips condition evaluation — every signal triggers
	// the action. Used for CHAT conversations and unconditional monitoring
	// streams. ADR-0032.
	ConditionTypeAlways = "always"
)

// WatchAction describes what the ReactiveEngine executes when a condition is met.
type WatchAction struct {
	// Type is "dispatch_agent", "emit_event", "start_plan", or "ingest".
	Type string `json:"type"`
	// TargetType is required when Type == "dispatch_agent".
	// "agent_id" → direct call; "capability" → full Gatekeeper+Auction.
	TargetType string `json:"target_type,omitempty"`
	// Target is the agent ID, capability description, metadata tag (ingest),
	// or event type override (emit_event).
	Target string `json:"target,omitempty"`
	// Payload is a template string with {{variable}} interpolation from the signal.
	Payload string `json:"payload,omitempty"`
}

// EffectiveActions returns every arm this watch runs, first arm first.
//
// One accessor so no caller has to remember the Action/Actions split, and an
// empty Action with populated Actions still works — which is what a client that
// only knows the plural form will send.
func (w WatchConfig) EffectiveActions() []WatchAction {
	var out []WatchAction
	if w.Action.Type != "" {
		out = append(out, w.Action)
	}
	out = append(out, w.Actions...)
	return out
}

// WatchConfig is a persistent user-defined reactive rule. When a signal
// arrives on the matching StreamID, the ReactiveEngine evaluates Condition
// and executes Action if the condition is true. ADR-0031 / ADR-0032.
type WatchConfig struct {
	ID            string      `json:"id"`
	Name          string      `json:"name,omitempty"`
	Description   string      `json:"description,omitempty"`
	Source        WatchSource `json:"source"`
	Condition     string      `json:"condition,omitempty"`      // e.g. "price > 5000" or "true"
	ConditionType string      `json:"condition_type,omitempty"` // see ConditionType* constants
	// Action is the FIRST arm, kept as the durable single-action form so every
	// persisted watch written before contract 0076 still loads unchanged.
	Action WatchAction `json:"action"`
	// Actions are arms 2..N (contract 0076). A multi-armed pipeline — "notify the
	// channel AND open a ticket" — could not be stored at all while Action was
	// singular, so the builder had to draw arms the engine would silently drop.
	//
	// Deliberately NOT a `repeated` replacement for Action: rewriting the field
	// would break every stored watch, and a migration for a feature nobody has
	// used yet is a cost with no payer. EffectiveActions() is the accessor
	// everything should read.
	Actions []WatchAction `json:"actions,omitempty"`
	Active        bool        `json:"active"`
	// ResponseMode is "" (async, default) or "sync" (CHAT conversations).
	ResponseMode string `json:"response_mode,omitempty"`
	// DaemonParams carries parameters injected into the daemon on first RegisterWatch.
	DaemonParams map[string]any `json:"daemon_params,omitempty"`
	// MaxConcurrentPlans limits simultaneous start_plan executions. Default 1.
	MaxConcurrentPlans int `json:"max_concurrent_plans,omitempty"`
	// DebounceSeconds coalesces a signal storm: when > 0, the watch fires at most
	// once per this many seconds, carrying the coalesced batch in the fired signal's
	// Payload. 0 disables debounce (fire on every signal). REACT-02 / ADR-0062.
	DebounceSeconds int `json:"debounce_seconds,omitempty"`
	// ConditionPayloadKeys is the allowlist of payload keys an `llm` condition may
	// read. When non-empty, the engine strips every other key from a copy of the
	// payload before it reaches the evaluator — shrinking the prompt-injection
	// surface to exactly the operator-intended fields. Empty ⇒ no filtering.
	// REACT-03 / ADR-0063.
	ConditionPayloadKeys []string `json:"condition_payload_keys,omitempty"`
	// Approved is the operator's explicit acknowledgement that a high-risk watch — an
	// `llm` condition driving a `start_plan`/`dispatch_agent` action, i.e. untrusted
	// content deciding an unattended consequential action — has been reviewed.
	// RegisterWatch rejects such a watch unless this is true. REACT-03 / ADR-0063.
	Approved bool `json:"approved,omitempty"`
	// DryRun (REACT-05 / ADR-0071) evaluates the condition and records what the watch
	// WOULD do, but never executes the action — so an operator can arm a watch in
	// observation mode ("would have fired 3× today") before letting it act.
	DryRun bool `json:"dry_run,omitempty"`
	// MissedFirePolicy (REACT-06 / ADR-0072) governs a "schedule" watch across a kernel
	// restart: "fire_once" fires a single catch-up if a scheduled time was missed while
	// down; "skip" (default) resumes at the next future time.
	MissedFirePolicy string `json:"missed_fire_policy,omitempty"`
}

// WatchMetrics is the per-watch observability snapshot (REACT-05 / ADR-0071): how many
// signals a watch saw, how often its condition fired vs was suppressed, dry-run
// would-fires, action failures/dead-letters, and mean condition-evaluation latency.
type WatchMetrics struct {
	WatchID               string
	SignalsSeen           int64
	ConditionFired        int64 // condition true → action attempted (or would-fire in dry-run)
	ConditionSuppressed   int64 // condition false
	DryRunWouldFire       int64
	ActionFailed          int64
	DeadLettered          int64
	ConditionEvalCount    int64
	ConditionLatencyMsTot int64
}

// MeanConditionLatencyMs returns the average condition-evaluation latency, 0 if none.
func (m WatchMetrics) MeanConditionLatencyMs() float64 {
	if m.ConditionEvalCount == 0 {
		return 0
	}
	return float64(m.ConditionLatencyMsTot) / float64(m.ConditionEvalCount)
}

// WatchBacktestVerdict is one signal's outcome when a candidate WatchConfig is replayed
// over the journal (REACT-05 / ADR-0071): would the condition have fired?
type WatchBacktestVerdict struct {
	Seq       uint64
	StreamID  string
	RawText   string
	WouldFire bool
	EvalError string
}
