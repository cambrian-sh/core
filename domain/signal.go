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

// WatchSource identifies the origin of signals for a WatchConfig.
type WatchSource struct {
	// Type is "daemon", "filesystem", "webhook", or "signal_stream".
	Type string
	// StreamID is the identifier that arriving signals must carry to match
	// this WatchConfig. For filesystem sources, it is the watched directory path.
	StreamID string
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

// WatchConfig is a persistent user-defined reactive rule. When a signal
// arrives on the matching StreamID, the ReactiveEngine evaluates Condition
// and executes Action if the condition is true. ADR-0031 / ADR-0032.
type WatchConfig struct {
	ID            string      `json:"id"`
	Name          string      `json:"name,omitempty"`
	Description   string      `json:"description,omitempty"`
	Source        WatchSource `json:"source"`
	Condition     string      `json:"condition,omitempty"` // e.g. "price > 5000" or "true"
	ConditionType string      `json:"condition_type,omitempty"` // see ConditionType* constants
	Action        WatchAction `json:"action"`
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
}
