package domain

import (
	"context"
	"errors"
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
	Backtest(ctx context.Context, cfg WatchConfig, afterSeq uint64) (WatchBacktestResult, error)
}

// WatchBacktestResult is a backtest's verdicts together with the journal window
// they were computed over (GOV-02).
//
// The window is part of the RESULT rather than a separate query on purpose. Journal
// GC shortens replayable history, so "this watch would have fired twice" is only
// meaningful next to how much history was actually searched — and a window a caller
// has to remember to ask for separately is a window most callers will not ask for.
type WatchBacktestResult struct {
	Verdicts []WatchBacktestVerdict
	// RetainedOldestSeq/RetainedNewestSeq bound the journal still on disk.
	RetainedOldestSeq uint64
	RetainedNewestSeq uint64
	// RetainedCount is how many records remain; 0 means an empty journal, which is
	// a stated answer rather than an unknown one.
	RetainedCount int
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
	Active  bool          `json:"active"`
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

// PipelineSummary is one authored reactive-pipeline revision, as the operator
// console renders it (contract 0087, ADR-0114 D33/D34).
//
// A read-model, deliberately: the authoring types live in the premium pipeline
// package and the console has no business knowing their shape. What it needs is
// what an operator looks at — which revision is live, what the graph costs, and
// which ingress mapping it was generated for.
type PipelineSummary struct {
	PipelineID string
	Revision   int
	Name       string
	// State is the D3 lifecycle position: draft, validated, published, armed, retired.
	State string
	// TriggerType is "ingress", "stream", "schedule" or "manual"; TriggerRef is
	// the ingress id, stream id or cron expression it names.
	TriggerType string
	TriggerRef  string
	NodeCount   int
	EdgeCount   int
	// EffectNodeCount is separated because it is the change class that can touch
	// the world — the number worth checking before arming.
	EffectNodeCount int
	// MappingRevision is the ingress mapping this graph was generated for, 0 for
	// a hand-authored pipeline bound to no mapping.
	MappingRevision int
	PlanChecksum    string
	// SemanticsChecksum is revision-independent, so two revisions differing only
	// cosmetically compare equal (ADR-0114 D29).
	SemanticsChecksum string
	// Generated is true when the Ingress Studio authored the revision rather
	// than an operator editing the canvas.
	Generated bool
	// DryRun evaluates everything and performs no effect (REACT-05).
	DryRun bool
	// Approved is the operator's acknowledgement for a high-risk pipeline.
	Approved bool
	// EntryLive reports whether the daemon feeding this pipeline is running.
	//
	// Three-valued as a pointer, because "not running" and "nobody can tell" are
	// different claims and only one of them is a problem. An armed pipeline whose
	// entry organ is switched off looks identical to a working one from the
	// pipeline store — the graph is armed either way — and that difference is the
	// whole question when a bot stops answering.
	EntryLive *bool
}

// PipelineLister reads authored reactive pipelines for the operator console.
//
// Same seam shape as WatchConfigHandler and for the same reason: the interface
// is named in domain so a downstream premium module can satisfy it, and it is
// nil in OSS builds, where the RPC returns an empty list rather than an error —
// "this build has no pipelines" is an answer, not a failure.
type PipelineLister interface {
	// ListPipelines returns authored revisions. armedOnly narrows to what is
	// live; ingressID narrows to one ingress, empty for all.
	ListPipelines(ctx context.Context, armedOnly bool, ingressID string) ([]PipelineSummary, error)
}

// ── Pipeline dry run (contract 0088) ────────────────────────────────────────

// PipelineDryRun is what a shadow run observed: everything the pipeline would
// have done, and nothing it did.
//
// The shape is deliberately four separate lists rather than one outcome per
// item. "47 saved" and "3 skipped because under the reporting threshold" answer
// different questions, and folding them into a single stream leaves an operator
// counting rows to learn what a heading could have told them.
type PipelineDryRun struct {
	RunID string
	// Samples is how many captured deliveries were replayed. A dry run over
	// zero samples is reported as such, never as a clean result.
	Samples int
	// Effects is what would have been carried out, in full — the ledger's
	// declared column. Its counterpart, the carried-out column, is always zero
	// here, and keeping them apart is what later distinguishes "the save
	// happened and the task failed" from "neither happened".
	Effects []ShadowEffectSummary
	// Duplicates is the finding a dry run exists for: two effects deriving the
	// same key means two writes silently collapsing into one, and after the
	// fact there is one record and no evidence there were ever two.
	Duplicates []DuplicateEffectKey
	// Terminations counts items that reached a declared discard, by the name the
	// operator gave it. Reported by name because "3 skipped because under the
	// reporting threshold" is a sentence an operator can check, and "47 of 52"
	// is one they have to trust.
	Terminations []PipelineTermination
	// Failures are items a node could not process — the red count.
	//
	// This is deliberately NOT "unrouted". An item reaching no port at all is
	// what the declared-termination rule makes structurally impossible: the
	// compiler refuses a graph with an unwired port, so that counter could only
	// ever read zero, and a counter that can never be anything else is noise
	// rather than a fact worth a column. What can still go wrong at runtime is a
	// node failing on an item, so that is what is counted and coloured.
	Failures []PipelineFailure
	// ElapsedMs is how long the replay took. Reported because a dry run is
	// something an operator waits for, and "4.1 s over 52 items" is the figure
	// that says whether running it over more is worth doing.
	ElapsedMs int64
	// Refused carries the named constraint when the run could not happen at all
	// (no such pipeline, nothing captured, a plan that will not compile). When
	// it is set every other field is empty, and the console shows this instead
	// of an empty report that looks like a clean one.
	Refused string
}

// ShadowEffectSummary is one effect that would have been carried out.
type ShadowEffectSummary struct {
	Node string
	Kind string
	// EffectKey is the real derived key, not a placeholder — which is what makes
	// a collision detectable here rather than weeks later.
	EffectKey string
	ItemKey   string
	// Summary is one line describing what it would have done.
	Summary string
}

// DuplicateEffectKey is one key more than one effect would have used.
type DuplicateEffectKey struct {
	EffectKey string
	Count     int
	// Nodes are the effect nodes that derived it. More than one node means two
	// distinct projections collapsing; one node means a fan-out whose key does
	// not separate the items it produced.
	Nodes []string
}

// PipelineTermination is one declared discard and how many items took it.
type PipelineTermination struct {
	Node string
	Port string
	// Reason is the operator's own words from the spec, which is why the
	// language refuses an unnamed discard: without a name this row can only say
	// that something was dropped.
	Reason string
	Count  int
}

// PipelineFailure is one item a node could not process.
type PipelineFailure struct {
	Node    string
	ItemKey string
	Err     string
}

// ── Pipeline authoring (contract 0089) ──────────────────────────────────────

// PipelineRefusal is one constraint a graph broke, and where it broke it.
//
// The target is what makes a refusal actionable rather than merely readable: a
// console draws it on the port it concerns and offers to jump to it. Recovering
// that from prose would mean parsing sentences, which breaks the first time one
// is improved.
type PipelineRefusal struct {
	// Constraint is the stable rule identifier. A console keys behaviour off it
	// — which refusals offer "wire it", which offer "terminate it with a
	// reason" — so it must not change when the wording does.
	Constraint string
	// Node and Port are empty when the refusal is about the pipeline or a whole
	// scope. A console offers "take me there" only when Node is set.
	Node string
	Port string
	// Message names the constraint, then the action. Never a validity verdict:
	// "invalid" tells an operator they are wrong and nothing about what to do,
	// and a graph that will not compile is usually one decision from compiling.
	Message string
}

// PipelineGraph is one authored revision, as authored.
type PipelineGraph struct {
	Summary PipelineSummary
	// GraphJSON is the ADR-0114 pipeline document.
	//
	// JSON rather than a mirrored proto message because the graph is recursive
	// (a structured-control node owns a nested scope) and every node carries an
	// open configuration map. Mirroring it would create a second schema to keep
	// in step with the first, and the drift would surface as a node kind the
	// console silently cannot render.
	GraphJSON string
	// Refusals is what the compiler says about this revision right now. Present
	// on a read because a stored revision can stop compiling — a ceiling
	// lowered, a node kind retired — and a console that only validated on edit
	// would show it as publishable.
	Refusals []PipelineRefusal
	// Reads maps a node id to the payload paths its expressions look at,
	// extracted from the compiled AST rather than from the source text.
	//
	// This is what turns a diagram of node kinds into a diagram of what happens
	// to the data: an operator comparing a step against a schema profile is
	// asking exactly this, and nothing else in the graph answers it.
	Reads map[string][]string
	// Refused is set when the revision cannot be read at all.
	Refused string
}

// PipelineValidation is the compiler's answer about a graph that is being
// edited and has not been stored.
type PipelineValidation struct {
	Refusals []PipelineRefusal
	// The plan facts a header shows once it compiles. Zero when it does not.
	NodeCount         int
	EffectNodeCount   int
	PlanChecksum      string
	SemanticsChecksum string
	// Refused is set when the document itself could not be read — malformed
	// JSON, say. Distinct from a refusal list, which means it WAS read and
	// broke rules.
	Refused string
}

// ErrPipelineNotFound means THIS source does not hold that pipeline — not that
// no source does.
//
// More than one source contributes pipelines: the reactive engine holds migrated
// watches and generated chat graphs, the Ingress Studio holds what it generates
// from mappings. A read names one pipeline, so it has to be offered to each in
// turn until one recognises it. Without a distinguishable "not mine" the
// composition root would have to match on refusal text, which breaks the first
// time a sentence is improved.
var ErrPipelineNotFound = errors.New("pipeline: not held by this source")

// PipelineAuthor reads and validates authored pipelines for the canvas.
//
// Validation is separate from the lifecycle's Validate on purpose: this one
// never stores anything and never transitions. An editor asks it on every
// change, and an editor that could accidentally publish is a worse editor.
type PipelineAuthor interface {
	// GetPipeline returns one revision. revision 0 means the latest authored.
	GetPipeline(ctx context.Context, pipelineID string, revision int) (PipelineGraph, error)
	// ValidatePipeline compiles a graph document without storing it.
	ValidatePipeline(ctx context.Context, graphJSON string) (PipelineValidation, error)
}

// PipelineSaved is the outcome of storing an edited graph.
type PipelineSaved struct {
	PipelineID string
	// Revision is the DRAFT revision that was created. Editing never overwrites
	// a revision that is published, armed or retired — a run pins a revision, so
	// mutating one underneath it would change what already happened.
	Revision int
	// Refusals is what the compiler said. A graph that breaks rules is still
	// STORED as a draft: an operator mid-edit needs to keep their work, and a
	// draft cannot run. What refusals block is publishing, which is a separate
	// act.
	Refusals []PipelineRefusal
	// Refused is set when nothing was stored at all.
	Refused string
}

// PipelineHolder is an OPTIONAL capability on a writer: it can say whether an id
// is already its own.
//
// It exists to route a save when more than one writer is registered. A read can
// be offered to each source until one recognises the id, but a save of a NEW
// pipeline is recognised by nobody — so the routing has to ask before writing
// rather than discover afterwards, or an edited studio pipeline would be stored
// into the reactive engine's own store and the two would disagree about what
// that pipeline is.
type PipelineHolder interface {
	HoldsPipeline(ctx context.Context, pipelineID string) (bool, error)
}

// PipelineWriter stores an edited graph as a new draft revision.
//
// Deliberately a SEPARATE port from PipelineAuthor rather than a third method on
// it. PipelineAuthor's guarantee is that it cannot write — that is what makes it
// safe to call on every keystroke — and adding a write method would quietly
// retract the guarantee its own documentation makes.
//
// It can create a draft and nothing else. Publishing and arming stay on the
// lifecycle, where each is an explicit act with its own gate: a canvas that
// could arm what it just drew would collapse four deliberate steps into one
// mouse click.
type PipelineWriter interface {
	SavePipeline(ctx context.Context, graphJSON string) (PipelineSaved, error)
}

// PipelineDryRunner shadow-runs an authored pipeline over captured deliveries.
//
// Nil in OSS, like PipelineLister — but unlike it, an OSS kernel REFUSES by name
// rather than answering with an empty report. "No pipelines exist" is a true
// answer to a listing; "nothing would happen" is not a true answer to a dry run
// on a build that cannot run one.
type PipelineDryRunner interface {
	// DryRunPipeline replays captured deliveries through the compiled plan with
	// every effect shadowed. revision 0 means the latest authored revision;
	// sampleLimit 0 means the implementation's default.
	DryRunPipeline(ctx context.Context, pipelineID string, revision, sampleLimit int) (PipelineDryRun, error)
}
