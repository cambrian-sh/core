package domain

import "time"

// Event type constants used with EventBus.Subscribe / EventBus.Publish.
const (
	EventTypeAgentReady     = "agent.ready"
	EventTypeAuctionEvent   = "auction.event"
	EventTypeSessionDormant = "session.dormant"
	// EventTypeSessionState reports the ABSOLUTE lifecycle state of a session after any
	// transition (open, pause, resume, dormant, complete). Phase 2 — it exists because
	// the two events above cover only two of the five transitions, so a consumer folding
	// just those can never see a session be created or paused.
	EventTypeSessionState = "session.state"
	// EventTypeConversationProgress is the ADR-0098 status line on the operator
	// feed. Live-only: it supersedes rather than accumulates.
	EventTypeConversationProgress = "conversation.progress"
	EventTypeMemoryPressure       = "memory.pressure"
	// EventTypeWatchTriggered is the default routing key for WatchTriggeredEvent.
	// WatchAction.Target can override it on a per-rule basis. ADR-0032.
	EventTypeWatchTriggered = "watch.triggered"
	// EventTypeDaemonCrashed is published when a daemon process exits unexpectedly. ADR-0033.
	EventTypeDaemonCrashed = "daemon.crashed"
	// EventTypeDaemonQuarantined is published when a crash-looping daemon is quarantined
	// (auto-restart withdrawn until manual intervention). REACT-04 / ADR-0070.
	EventTypeDaemonQuarantined = "daemon.quarantined"
	// EventTypeDaemonRecovered is published when a crashed daemon is successfully
	// auto-restarted; ReactiveEngine re-marks its stream available. REACT-04 / ADR-0070.
	EventTypeDaemonRecovered = "daemon.recovered"
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
	// EventTypeReactiveBudget reports that a reactive backpressure budget was
	// exhausted and load is being shed. REACT-02 / ADR-0062.
	EventTypeReactiveBudget = "reactive.budget"
	// EventTypeAgentStep reports one action inside an agent's ReAct loop (a
	// memory_query today) so the harness can diagnose agent-internal failure modes
	// the orchestration trace hides: query-thrash (loop length + near-duplicate
	// queries) and context poisoning (retrievals authored by the agent itself, or
	// pulled from a different session). Diagnostic only — zero behavior change.
	EventTypeAgentStep = "agent.step"
	// EventTypeRetentionRun reports one bounded retention/compaction pass: what was
	// deleted, whether the pass hit its cap, and whether it failed. ADR-0102 A1.
	//
	// Deliberately SOURCE-AGNOSTIC. Two callers have this exact shape — the records
	// plugin's version compactor (REC-03) and the reactive-journal GC (GOV-02) — and
	// a `drift_records`-specific event would both need a second contract bump for the
	// second caller and name a premium feature in an OSS proto (ADR-0057 D5).
	EventTypeRetentionRun = "retention.run"

	// EventTypePipelineMeters reports what a running pipeline is doing NOW —
	// per node, how many items entered, how many are in flight, and where they
	// left by.
	//
	// In-flight is the reason this is an event and not an RPC. A count of items
	// that passed a node says nothing about a node holding forty of them: the
	// totals look healthy right up until the queue is the whole problem, and
	// the number is meaningless once you read it after the fact.
	EventTypePipelineMeters = "pipeline.meters"

	// EventTypePipelineStep is one item moving through one node: which node,
	// which port it left by, how long it took, and — when tracing is on — what
	// it carried.
	//
	// Live-only and never replayed, like a token chunk. It is a view of what is
	// happening NOW; replaying a step from an hour ago into a flow view would
	// show motion that is not occurring.
	EventTypePipelineStep = "pipeline.step"
	// EventTypeAgentLLMExchange is one agent reasoning turn captured at the managed LLM
	// provider chokepoint: the full prompt+completion of a GenerateViaModelStream call.
	// The ordered sequence per session reconstructs an agent's whole internal ReAct loop
	// (every output + every loop step) with no SDK instrumentation. Best-effort, live-only,
	// never replayed; gated behind execution.capture_llm_exchanges. Diagnostic only.
	EventTypeAgentLLMExchange = "agent.llm_exchange"
)

// DomainEvent is the sealed interface for all internal system events.
// All implementations must live in this package (sealed by domainEvent()).
type DomainEvent interface {
	domainEvent()
	// EventType returns the routing key used by EventBus to dispatch to
	// subscribers. Must match one of the EventType* constants above.
	EventType() string
}

// SelectionEventPayload reports bidding lifecycle (started / completed / failed).
// Emitted by the Dispatcher via EventBus.
//
// WinnerMargin and Funnel are ROUTE-02 routing-trace fields: they make a
// mis-routed step explainable from the persisted event alone (the candidate
// funnel that produced the slate, and how decisively the winner beat the
// runner-up). Both are best-effort — Funnel is nil when routing tracing is
// disabled (config execution.routing_trace_enabled) or on the "started" event.
type SelectionEventPayload struct {
	TaskID   string
	TaskDesc string
	Status   string
	WinnerID string
	Bids     []CandidateEntry
	ErrorMsg string
	// WinnerMargin is the winning bid's confidence minus the highest-confidence
	// losing bid (0 when there is no runner-up). A near-zero margin flags a
	// coin-flip auction; a wide margin, a decisive one.
	WinnerMargin float32
	// Funnel is the Gatekeeper's per-agent Declaration→Interview→Merit trace for
	// this auction (ROUTE-02). Nil when tracing is off or not applicable.
	Funnel *GatekeeperFunnel

	// SelectionLatencyMs is the wall-time spent DECIDING who runs this step —
	// candidate discovery plus (on the auction arm) the bid round. It excludes
	// the winner's actual execution. ADR-0100 P2.
	SelectionLatencyMs int32
	// SelectionBoots is the number of agent processes cold-started to reach that
	// decision. The auction booted every candidate to ask it for a bid; dispatch
	// boots only the winner, and boots nothing at all to decide. ADR-0100 P2.
	SelectionBoots int32
}

// CandidateEntry is a single agent's bid inside an SelectionEventPayload.
type CandidateEntry struct {
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
// SelectionEventPayload so a suite row can reconstruct why a step routed the way
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
	// components, in the order presented to the Dispatcher (highest first).
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

	// Contract 0074: the agent detail fields contract 0057 removed from the
	// operator projection. Carried on the READY event because the projection
	// folds agents by id from it — putting them here means an agent detail pane
	// fills from the feed rather than needing a per-entity getter back.
	Description        string
	Trait              string
	Runtime            string
	ExecPath           string
	ManifestVersion    string
	Provisional        bool
	System             bool
	ClassificationTags []string
	// LastError is the most recent failure this agent reported. A healthy-looking
	// agent that fails every dispatch is otherwise indistinguishable from an idle
	// one.
	LastError string
}

// SessionDormantEvent is emitted by SessionManager when a session transitions
// to the Dormant state. MemoryLifecycleManager subscribes to schedule
// per-session consolidation. ADR-0030.
type SessionDormantEvent struct {
	SessionID   SessionID
	DormantAt   time.Time
	TTLDuration time.Duration
}

// SessionStateEvent is the absolute lifecycle state of a session, published by
// SessionManager on every transition INCLUDING creation. Phase 2.
//
// Consumers upsert by SessionID; there is no history to replay and no ordering requirement
// beyond the feed's own sequence. Reason is set only for operator-driven transitions.
type SessionStateEvent struct {
	SessionID SessionID
	Status    SessionStatus
	Goal      string
	ParentID  SessionID
	CreatedAt time.Time
	UpdatedAt time.Time
	Reason    string
}

func (SessionStateEvent) EventType() string { return EventTypeSessionState }

// ConversationProgressEvent carries an ADR-0098 progress snapshot onto the
// OPERATOR feed (contract 0079).
//
// The progress channel delivers to an ingress-bound conversation, so Telegram
// saw "working on it, step 2 of 4" and the operator console saw nothing — a
// console conversation has no delivery address, so DeliverProgress returned
// early and the snapshot was computed and dropped.
//
// It rides the EPHEMERAL lane (seq 0, never replayed) because it is a status
// line rather than history. A replayed "working on it" for a turn that finished
// an hour ago would be worse than showing no progress at all.
type ConversationProgressEvent struct {
	ConversationID string
	// Text is the rendered line. EMPTY means CLEAR — a final update with nothing
	// to say takes the line down.
	Text       string
	Phase      string
	Step       int
	TotalSteps int
	Final      bool
	UpdatedAt  time.Time
}

func (ConversationProgressEvent) domainEvent()      {}
func (ConversationProgressEvent) EventType() string { return EventTypeConversationProgress }

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

// DaemonQuarantinedEvent is published when a crash-looping daemon exceeds its restart
// budget and is quarantined — no further auto-restart, its watches are degraded until an
// operator intervenes. REACT-04 / ADR-0070.
type DaemonQuarantinedEvent struct {
	AgentID  string
	StreamID string
	Reason   string
	Attempts int
}

// DaemonRecoveredEvent is published when a crashed daemon is successfully auto-restarted.
// REACT-04 / ADR-0070.
type DaemonRecoveredEvent struct {
	AgentID  string
	StreamID string
}

func (DaemonQuarantinedEvent) domainEvent()      {}
func (DaemonQuarantinedEvent) EventType() string { return EventTypeDaemonQuarantined }
func (DaemonRecoveredEvent) domainEvent()        {}
func (DaemonRecoveredEvent) EventType() string   { return EventTypeDaemonRecovered }

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
	// Steps is the plan's DAG for live rendering (operator UI): one node per step with
	// its dependency edges and current execution status. Absolute-state like the rest of
	// this event — the full node set is re-sent on each update so re-delivery folds
	// idempotently and a late subscriber gets the whole graph. Empty on legacy/synthetic
	// emitters that have no plan structure (backward compatible).
	Steps []PlanStepState
}

// PlanStepState is one node of a plan's DAG on the operator feed: its position in the
// plan, a short label, its dependency edges, and its live execution status.
type PlanStepState struct {
	Index     int
	Label     string // short human label (the step's query, truncated for display)
	DependsOn []int  // zero-based indices of steps that must finish before this one
	IsThought bool   // a reasoning/synthesis step (no external action)
	Status    string // "pending" | "running" | "done" | "failed"
	Agent     string // the executor agent id, once the step has been dispatched
	// RequiredCapabilities is the step's ROUTE-03 capability contract, copied from
	// domain.Step. Carried onto the feed (contract 0072) because it is the REASON
	// a step routed where it did — Agent is only the result. When routing fails,
	// "no agents available" without the requested capability is a dead end for
	// whoever has to fix it.
	RequiredCapabilities []string
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

// AgentLLMExchangeEvent carries the full prompt+completion of one agent reasoning
// turn, captured at the managed LLM provider chokepoint (the Langfuse tap). Best-effort,
// live-only, never replayed; the emitter truncates Prompt/Completion and records the
// untruncated lengths. Gated behind execution.capture_llm_exchanges. ADR-0079.
type AgentLLMExchangeEvent struct {
	SessionID     string
	AgentID       string
	StepIndex     int
	Purpose       string
	ModelID       string
	Prompt        string
	Completion    string
	PromptChars   int
	ResponseChars int
}

func (AgentLLMExchangeEvent) domainEvent()      {}
func (AgentLLMExchangeEvent) EventType() string { return EventTypeAgentLLMExchange }

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

func (SelectionEventPayload) domainEvent() {}
func (AgentReadyEvent) domainEvent()       {}
func (SessionStateEvent) domainEvent()     {}
func (SessionDormantEvent) domainEvent()   {}
func (MemoryPressureEvent) domainEvent()   {}
func (WatchTriggeredEvent) domainEvent()   {}
func (DaemonCrashedEvent) domainEvent()    {}

func (SelectionEventPayload) EventType() string { return EventTypeAuctionEvent }
func (AgentReadyEvent) EventType() string       { return EventTypeAgentReady }
func (SessionDormantEvent) EventType() string   { return EventTypeSessionDormant }
func (MemoryPressureEvent) EventType() string   { return EventTypeMemoryPressure }

func (DaemonCrashedEvent) EventType() string { return EventTypeDaemonCrashed }

// EventType returns "watch.triggered" unless ActionTarget overrides it.
func (e WatchTriggeredEvent) EventType() string {
	if e.ActionTarget != "" {
		return e.ActionTarget
	}
	return EventTypeWatchTriggered
}

// RetentionDeletion is one category of thing a retention pass removed. Carried as
// a list rather than as fixed fields because what a pass deletes differs per
// source (record versions and push commands; journal entries and dead letters),
// and a fixed pair of int columns would force every future source to either
// misname its counts or break the contract to add its own.
type RetentionDeletion struct {
	// Category is the caller's own word for what was removed, e.g.
	// "record_versions", "push_commands", "journal_entries".
	Category string
	Count    int
}

// PipelineMetersEvent is a live per-node reading for one pipeline.
//
// Process-local and NOT durable, deliberately: it describes what this kernel is
// doing now. History is the journal's job, and a counter that survived a restart
// would claim continuity across a boundary the runtime does not have.
type PipelineMetersEvent struct {
	PipelineID string
	Revision   int
	Nodes      []PipelineNodeMeter
	At         time.Time
}

// PipelineStepEvent is one item passing through one node.
//
// The meters answer "how much and how fast"; this answers "what went where".
// Both are needed and neither substitutes: an operator watching a graph wants
// the counts to spot a queue and the steps to see an individual item take the
// branch they did not expect.
//
// # Why the payload is opt-in
//
// An item's value is the actual data flowing through the deployment. Shipping it
// to the operator plane by default would make a diagnostic view an exfiltration
// surface, so it travels only when tracing is explicitly enabled, and it is
// truncated by the emitter with the untruncated size reported — so a reader can
// never mistake a clipped payload for the whole of one.
type PipelineStepEvent struct {
	PipelineID string
	Revision   int
	RunID      string
	// ItemKey identifies the item across steps, which is what makes a sequence
	// of these a trace rather than a list.
	ItemKey string
	Node    string
	Kind    string
	// Port is the port it left by, empty when it did not leave by one.
	Port string
	// Outcome is "routed", "terminated" or "failed".
	Outcome string
	// Reason is the operator's own words for a declared discard, or the error.
	Reason     string
	DurationMs int64
	At         time.Time
	// Payload is a bounded preview of what the item carried, present only when
	// tracing is enabled.
	Payload string
	// PayloadBytes is the UNTRUNCATED size, so truncation is visible rather than
	// silent.
	PayloadBytes int
	// Dropped counts steps this pipeline produced since the last emitted one and
	// did not send, because the trace is rate-capped. Reported rather than
	// hidden: a flow view that quietly skips is one that lies about the shape of
	// the traffic.
	Dropped int64
}

// Pipeline step outcomes.
const (
	StepRouted     = "routed"
	StepTerminated = "terminated"
	StepFailed     = "failed"
)

// PipelineNodeMeter is one node's live reading.
type PipelineNodeMeter struct {
	Node string
	// Entered is how many items reached this node.
	Entered int64
	// InFlight is how many are being worked right now — the bottleneck signal,
	// and the one figure that cannot be reconstructed afterwards.
	InFlight int64
	// Failed is items the node could not process. Kept apart from an `error`
	// port: "could not process it" and "sent it down error" are different
	// events with different fixes.
	Failed int64
	// PassedOn counts items that completed and were routed onward without a
	// terminal outcome — the ordinary path through a node. Distinct from Ports,
	// which only a terminal fate names.
	PassedOn int64
	// Ports counts items by the port they left on.
	Ports map[string]int64
	// MeanLatencyMs is enough to spot the node an order of magnitude slower
	// than its neighbours, which is what "where is it stuck" usually means.
	MeanLatencyMs int64
}

// RetentionRunEvent reports one bounded retention/compaction pass so an operator
// can see what was dropped and when (ADR-0102 Amendment A1).
//
// A1 reverses ADR-0102 D6, which put retention runs only on the owning plugin's
// own plane. Deletion that is answerable only if you already know to go looking
// is too close to silent for a product whose argument is auditability — the
// operator watching the feed is exactly the person who needs to know the audit
// trail just got shorter.
//
// A FAILED pass is an event too. Retention that stops working quietly is how a
// table becomes the outage, so `Err` being set is the signal worth surfacing
// most, not an error to swallow.
type RetentionRunEvent struct {
	// Source identifies which retention domain ran, e.g. "records". Free-form by
	// design: the OSS feed must not enumerate premium plugins.
	Source string
	// RunID is the source's own identifier for the pass, so a feed entry can be
	// joined to the durable row behind it.
	RunID     int64
	StartedAt time.Time
	// FinishedAt is when the pass ended, successfully or not.
	FinishedAt time.Time
	// Deleted is the per-category count. Empty means the pass ran and removed
	// nothing — which is a normal, reportable outcome, not an absence of data.
	Deleted []RetentionDeletion
	// Bounded is true when the pass hit its per-pass cap and more remains. An
	// operator seeing this on every tick is being told the backlog is not draining.
	Bounded bool
	// Err is empty on success.
	Err string
}

func (RetentionRunEvent) domainEvent()        {}
func (PipelineMetersEvent) domainEvent()      {}
func (PipelineStepEvent) domainEvent()        {}
func (RetentionRunEvent) EventType() string   { return EventTypeRetentionRun }
func (PipelineMetersEvent) EventType() string { return EventTypePipelineMeters }
func (PipelineStepEvent) EventType() string   { return EventTypePipelineStep }

// AgentStepEvent is one observed step of an agent's in-loop activity (a memory_query
// today), emitted so the benchmark harness can measure what the final Handoff hides:
// query-thrash (how many queries an agent fires, and how similar they are — the
// budget-exhaustion failure mode) and context poisoning (SelfHits: results the agent
// itself authored, a self-referential feedback loop; CrossSessionHits: results written
// in a different session bleeding in). Diagnostic only — zero behavior change.
type AgentStepEvent struct {
	SessionID        string
	AgentID          string
	Action           string // "memory_query" (extensible to tool_call, find_tools, ...)
	Query            string // query text (or tool name) — for thrash/near-duplicate detection
	Hits             int    // number of results returned
	SelfHits         int    // results authored by AgentID (self-referential poisoning)
	CrossSessionHits int    // results written in a DIFFERENT session (cross-session bleed)
}

func (AgentStepEvent) domainEvent()      {}
func (AgentStepEvent) EventType() string { return EventTypeAgentStep }

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
