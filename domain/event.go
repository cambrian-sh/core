package domain

import "time"

// Event type constants used with EventBus.Subscribe / EventBus.Publish.
const (
	EventTypeAgentReady       = "agent.ready"
	EventTypeAuctionEvent     = "auction.event"
	EventTypeSessionDormant   = "session.dormant"
	EventTypeSessionCompleted = "session.completed"
	EventTypeMemoryPressure   = "memory.pressure"
	// EventTypeWatchTriggered is the default routing key for WatchTriggeredEvent.
	// WatchAction.Target can override it on a per-rule basis. ADR-0032.
	EventTypeWatchTriggered = "watch.triggered"
	// EventTypeDaemonCrashed is published when a daemon process exits unexpectedly. ADR-0033.
	EventTypeDaemonCrashed = "daemon.crashed"
	// ADR-0047 operator-feed events. Producers publish these on the EventBus;
	// the operator plane is a pure consumer. Payloads are absolute-state (D6).
	EventTypeMemoryWritten = "memory.written"
	EventTypeHITLRaised    = "hitl.raised"
	EventTypeVerifierRound = "verifier.round"
	EventTypeLLMHealth     = "llm.health"
	// EventTypePlanState reports a plan/step state transition (absolute-state).
	// The operator plane folds these into its live "Plans in Flight" projection
	// (no kernel PlanRegistry). ADR-0047 D7.
	EventTypePlanState = "plan.state"
	// EventTypeAudit reports an operator-mutating action. ADR-0047 D15.
	EventTypeAudit = "operator.audit"
	// EventTypeTokenChunk is a best-effort, live-only step-output fragment. It is
	// NEVER spooled/replayed (a reconnecting client resyncs accumulated text from
	// the snapshot/ContentStore). ADR-0047 D5/D12.
	EventTypeTokenChunk = "token.chunk"
	// EventTypeWorldDelta reports that a READ observation found a world-model entity
	// field changed from its cached value — i.e. the world moved outside our action
	// (ADR-0049 §A1.2 / ADR-0051 D3). PASSIVE: the entity is updated and this signal
	// emitted; there is no propagation or in-loop rescan. Durable raw material for
	// deferred adaptive per-entity trust (ADR-0037 selection layer).
	EventTypeWorldDelta = "world.delta"
)

// DomainEvent is the sealed interface for all internal system events.
// All implementations must live in this package (sealed by domainEvent()).
type DomainEvent interface {
	domainEvent()
	// EventType returns the routing key used by EventBus to dispatch to
	// subscribers. Must match one of the EventType* constants above.
	EventType() string
}

// AuctionEventPayload reports bidding lifecycle (started / completed / failed).
// Emitted by Auctioneer via EventBus.
type AuctionEventPayload struct {
	TaskID   string
	TaskDesc string
	Status   string
	WinnerID string
	Bids     []BidEntry
	ErrorMsg string
}

// BidEntry is a single agent's bid inside an AuctionEventPayload.
type BidEntry struct {
	AgentID    string
	Confidence float32
	Rationale  string
	LatencyMs  int32
	IsTool     bool
}

// AgentReadyEvent is emitted by InterviewWorker after every Provisional→Active
// transition. Subscribers (CapabilityClusterer, SynapticWatcher, etc.) can react
// to new agents without polling. ADR-0023 D6A.
type AgentReadyEvent struct {
	AgentID      string
	SourceHash   string
	TrustScore   float64
	Capabilities []string
	InterviewMs  int64
}

// SessionDormantEvent is emitted by SessionManager when a session transitions
// to the Dormant state. MemoryLifecycleManager subscribes to schedule
// per-session consolidation. ADR-0030.
type SessionDormantEvent struct {
	SessionID   string
	DormantAt   time.Time
	TTLDuration time.Duration
}

// SessionCompletedEvent is emitted by MemoryLifecycleManager after consolidation
// finishes for a dormant session. ADR-0030.
type SessionCompletedEvent struct {
	SessionID       string
	ConsolidatedAt  time.Time
	DocumentsMerged int
}

// MemoryPressureEvent is emitted when document count or index size exceeds a
// configured threshold. Subscribers (MemoryLifecycleManager, Scavenger) trigger
// cleanup in response. ADR-0030.
type MemoryPressureEvent struct {
	TotalDocuments int
	IndexSizeBytes int64
	Trigger        string // ConsolidationTrigger constant
}

// DaemonCrashedEvent is published by AgentManager when a daemon process exits
// unexpectedly (not via StopDaemon). ReactiveEngine subscribes to mark the
// stream unavailable. ADR-0033.
type DaemonCrashedEvent struct {
	AgentID  string
	StreamID string
}

// WatchTriggeredEvent is emitted by the ReactiveEngine when a WatchConfig
// condition evaluates to true and the action is executed. SynapticWatcher
// is the implicit first subscriber (priority 7). ADR-0032.
type WatchTriggeredEvent struct {
	WatchConfigID string
	StreamID      string
	SignalPayload map[string]any
	// ActionTarget overrides the published EventType when non-empty,
	// allowing per-rule custom routing keys.
	ActionTarget string
}

// MemoryWrittenEvent reports a write to the LTM (a new/superseded document).
// Absolute-state: it names the resulting document, not a delta. ADR-0047 D3.
type MemoryWrittenEvent struct {
	DocID     string
	DocType   string
	SessionID string
	Source    string
	Summary   string
}

// HITLRaisedEvent reports that an execution paused for a human-in-the-loop
// decision (a dangerous tool / destructive command). ADR-0047 D3.
type HITLRaisedEvent struct {
	InterventionID string
	SessionID      string
	AgentID        string
	Description    string
	IsDestructive  bool
}

// VerifierRoundEvent reports the outcome of a verification round. ADR-0047 D3.
type VerifierRoundEvent struct {
	TaskID       string
	WinnerAgent  string
	QualityScore float64
	BidConf      float64
	Critique     string
}

// LLMHealthEvent reports an LLM-provider health/circuit-breaker transition for
// a model id. Absolute-state: it carries the new state, not a delta. ADR-0047 D3.
type LLMHealthEvent struct {
	ModelID string
	State   string // "closed" | "open" | "half_open"
	Reason  string
}

// PlanStateChanged reports the absolute state of a plan step. The operator
// projection upserts by PlanID and drops the plan when Terminal is true (the
// plan completed/failed/aborted). Absolute-state: CostSoFar/ActiveStep are
// totals, not deltas, so re-delivery on resume folds idempotently. ADR-0047 D6/D7.
type PlanStateChanged struct {
	SessionID   string
	PlanID      string
	ActiveStep  int
	Status      string // "running" | "completed" | "failed" | "aborted" | "replanning"
	ActiveAgent string
	CostSoFar   float64
	Terminal    bool
}

// AuditEvent carries an operator-mutating action onto the feed in realtime
// (ADR-0047 D15). Emitted only after the AuditEntry is durably recorded
// (write-then-emit), so a client folding it always finds the row.
type AuditEvent struct {
	Entry AuditEntry
}

func (MemoryWrittenEvent) domainEvent() {}
func (HITLRaisedEvent) domainEvent()    {}
func (VerifierRoundEvent) domainEvent() {}
func (LLMHealthEvent) domainEvent()     {}
func (PlanStateChanged) domainEvent()   {}
func (AuditEvent) domainEvent()         {}
func (AuditEvent) EventType() string    { return EventTypeAudit }

// TokenChunkEvent is a best-effort, live-only fragment of a step's streamed
// output (managed-proxy generations only). Never replayed. ADR-0047 D12.
type TokenChunkEvent struct {
	SessionID string
	StepIndex int
	Text      string
}

func (TokenChunkEvent) domainEvent()      {}
func (TokenChunkEvent) EventType() string { return EventTypeTokenChunk }

// WorldDeltaEvent reports a single entity field whose value a READ observation found
// changed from its cached state (ADR-0049 §A1.2). Absolute-state: it names the entity,
// field, and the new value (Old is carried for diagnostics). Passive — emitted after the
// entity is updated; consumers (telemetry/operator, later adaptive-trust mining) react,
// nothing in the write path blocks on it.
type WorldDeltaEvent struct {
	EntityKey  string // canonical kind:id
	Kind       string
	Field      string // the changed field (e.g. "content_ref", "exists")
	OldValue   string
	NewValue   string
	ObservedAt time.Time
	SessionID  string
}

func (WorldDeltaEvent) domainEvent()      {}
func (WorldDeltaEvent) EventType() string { return EventTypeWorldDelta }

func (MemoryWrittenEvent) EventType() string { return EventTypeMemoryWritten }
func (HITLRaisedEvent) EventType() string    { return EventTypeHITLRaised }
func (VerifierRoundEvent) EventType() string { return EventTypeVerifierRound }
func (LLMHealthEvent) EventType() string     { return EventTypeLLMHealth }
func (PlanStateChanged) EventType() string   { return EventTypePlanState }

func (AuctionEventPayload) domainEvent()   {}
func (AgentReadyEvent) domainEvent()       {}
func (SessionDormantEvent) domainEvent()   {}
func (SessionCompletedEvent) domainEvent() {}
func (MemoryPressureEvent) domainEvent()   {}
func (WatchTriggeredEvent) domainEvent()   {}
func (DaemonCrashedEvent) domainEvent()    {}

func (AuctionEventPayload) EventType() string  { return EventTypeAuctionEvent }
func (AgentReadyEvent) EventType() string      { return EventTypeAgentReady }
func (SessionDormantEvent) EventType() string  { return EventTypeSessionDormant }
func (SessionCompletedEvent) EventType() string { return EventTypeSessionCompleted }
func (MemoryPressureEvent) EventType() string  { return EventTypeMemoryPressure }

func (DaemonCrashedEvent) EventType() string { return EventTypeDaemonCrashed }

// EventType returns "watch.triggered" unless ActionTarget overrides it.
func (e WatchTriggeredEvent) EventType() string {
	if e.ActionTarget != "" {
		return e.ActionTarget
	}
	return EventTypeWatchTriggered
}
