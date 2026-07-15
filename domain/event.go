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
	// EventTypeScoutUsefulness reports, per session, whether the always-on Scout's
	// pre-plan discovery actually earned its cost (ROUTE-08 phase A): was the
	// <DiscoveryLTM> referenced by the plan, did the plan run without replan, and
	// what did the Scout cost. Logging only — the raw material to later learn a
	// self-regulation (invoke/skip) policy (phase B).
	EventTypeScoutUsefulness = "scout.usefulness"
	// EventTypeReactiveBudget reports that a reactive backpressure budget was
	// exhausted and load is being shed. REACT-02 / ADR-0062.
	EventTypeReactiveBudget = "reactive.budget"
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
//
// WinnerMargin and Funnel are ROUTE-02 routing-trace fields: they make a
// mis-routed step explainable from the persisted event alone (the candidate
// funnel that produced the slate, and how decisively the winner beat the
// runner-up). Both are best-effort — Funnel is nil when routing tracing is
// disabled (config execution.routing_trace_enabled) or on the "started" event.
type AuctionEventPayload struct {
	TaskID   string
	TaskDesc string
	Status   string
	WinnerID string
	Bids     []BidEntry
	ErrorMsg string
	// WinnerMargin is the winning bid's confidence minus the highest-confidence
	// losing bid (0 when there is no runner-up). A near-zero margin flags a
	// coin-flip auction; a wide margin, a decisive one.
	WinnerMargin float32
	// Funnel is the Gatekeeper's per-agent Declaration→Interview→Merit trace for
	// this auction (ROUTE-02). Nil when tracing is off or not applicable.
	Funnel *GatekeeperFunnel
}

// BidEntry is a single agent's bid inside an AuctionEventPayload.
type BidEntry struct {
	AgentID    string
	Confidence float32
	Rationale  string
	LatencyMs  int32
	IsTool     bool
	// Requirements are the dependencies the agent declared it needs satisfied
	// before it can execute (ROUTE-02 — part of the auction proposal record).
	Requirements []string
}

// GatekeeperFunnel is the per-auction candidate funnel: every agent the
// Gatekeeper considered and the layer that admitted or eliminated it
// (ROUTE-02). Produced by Gatekeeper.FindCandidates and carried to the
// AuctionEventPayload so a suite row can reconstruct why a step routed the way
// it did. Pure domain — no proto, no infrastructure.
type GatekeeperFunnel struct {
	// L1 is the Declaration pass-set: one entry per agent considered, with the
	// pass/fail verdict and reason.
	L1 []DeclarationResult
	// L2 is the Interview (semantic) layer: survivors and eliminated agents with
	// their similarity scores. Empty when Layer 2 did not run (e.g. no embedder,
	// or only provisional candidates).
	L2 []InterviewResult
	// L2Threshold is the similarity floor applied in Layer 2 (0 when L2 skipped).
	L2Threshold float64
	// L3 is the Merit ranking: the surviving candidates with their score and its
	// components, in the order presented to the Auctioneer (highest first).
	L3 []MeritResult
	// MaxCandidates is the GatekeeperMaxCandidates cap applied after ranking
	// (0 when uncapped).
	MaxCandidates int
}

// DeclarationResult records one agent's Layer-1 (Declaration) verdict.
type DeclarationResult struct {
	AgentID string
	Passed  bool
	Reason  string // why it failed (empty when Passed)
}

// InterviewResult records one agent's Layer-2 (Interview) verdict. Similarity
// is the best embedding similarity of the agent's profile to the task; Survived
// reflects whether it cleared the threshold. ProvisionalBypass is true when the
// agent skipped the semantic gate because it is provisional (cold-start pass).
type InterviewResult struct {
	AgentID           string
	Similarity        float64
	Survived          bool
	ProvisionalBypass bool
}

// MeritResult records one candidate's Layer-3 (Merit) score and its components,
// mirroring the GatekeeperScore formula so a reviewer can see which term drove
// the ranking.
type MeritResult struct {
	AgentID     string
	Score       float64
	SuccessRate float64
	TrustScore  float64
	LatencyTerm float64 // w3 * (1 / normLatency) contribution
	CostTerm    float64 // w4 * normalizedCost contribution (subtracted)
	Provisional bool    // provisional cold-start penalty applied
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

func (AuctionEventPayload) EventType() string   { return EventTypeAuctionEvent }
func (AgentReadyEvent) EventType() string       { return EventTypeAgentReady }
func (SessionDormantEvent) EventType() string   { return EventTypeSessionDormant }
func (SessionCompletedEvent) EventType() string { return EventTypeSessionCompleted }
func (MemoryPressureEvent) EventType() string   { return EventTypeMemoryPressure }

func (DaemonCrashedEvent) EventType() string { return EventTypeDaemonCrashed }

// EventType returns "watch.triggered" unless ActionTarget overrides it.
func (e WatchTriggeredEvent) EventType() string {
	if e.ActionTarget != "" {
		return e.ActionTarget
	}
	return EventTypeWatchTriggered
}

// ScoutUsefulnessEvent is the ROUTE-08 phase-A per-session signal: did the
// always-on Scout's pre-plan discovery pay for itself? Emitted once after
// execution. Logging only (zero behavior change) — the training material for a
// later invoke/skip policy (phase B).
type ScoutUsefulnessEvent struct {
	SessionID string
	// ScoutRan is false when the Scout was disabled/absent or produced an empty
	// (degrade-to-one-shot) report — the baseline the useful case is compared to.
	ScoutRan bool
	// ScoutLatencyMs is the wall-clock cost of the discovery pass (0 when it did
	// not run).
	ScoutLatencyMs int64
	// DiscoveryEntities is how many structured observations the Scout returned.
	DiscoveryEntities int
	// DiscoveryReferenced is true when the emitted plan textually referenced the
	// discovery (entity id/kind/summary token or an environment path) — the proxy
	// for "the discovery changed the plan" (spec: string/citation overlap).
	DiscoveryReferenced bool
	// PlanSteps is the number of steps in the emitted plan.
	PlanSteps int
	// ReplanCount is how many times execution had to replan; Replanned is
	// ReplanCount > 0. A plan that ran without replan is the "discovery was
	// sufficient" success signal.
	ReplanCount int
	Replanned   bool
}

func (ScoutUsefulnessEvent) domainEvent()      {}
func (ScoutUsefulnessEvent) EventType() string { return EventTypeScoutUsefulness }

// ReactiveBudgetEvent is emitted when a reactive backpressure budget is exhausted
// and the engine sheds load (skips + dead-letters the shed unit) — so budget
// exhaustion is operator-visible, not a silent stall. Throttled to at most once per
// minute per resource. REACT-02 / ADR-0062.
type ReactiveBudgetEvent struct {
	// Resource is the exhausted budget: "llm_condition", "start_plan", or "stream_rate".
	Resource string
	// Reason is the dead-letter reason applied to shed units
	// ("budget_exhausted", "plan_budget_exhausted", "rate_limited").
	Reason string
	// StreamID is the affected stream ("" for plane-wide budgets).
	StreamID string
	// SheddingSince is when shedding for this resource began (this window).
	SheddingSince time.Time
}

func (ReactiveBudgetEvent) domainEvent()      {}
func (ReactiveBudgetEvent) EventType() string { return EventTypeReactiveBudget }
