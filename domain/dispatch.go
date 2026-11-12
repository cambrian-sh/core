package domain

import (
	"context"
	"time"
)

// Payload is the domain-layer mirror of proto's Object message.
type Payload struct {
	ID       string
	Type     string
	Data     []byte
	Metadata map[string]string
}

// Handoff is the domain-layer mirror of proto's Handoff message.
// It carries a task between agents through the execution DAG.
type Handoff struct {
	ID            string
	FromAgent     string
	ToAgent       string
	Payload       *Payload
	Confidence    float32
	Uncertainties []string
	Context       map[string]string // Phase 0/1/2 and circuit-breaker fallback
	BudgetLease   *BudgetLease      // per-step managed LLM session (ADR-0018); nil-safe
	// WorkingMemory is populated in Phase 3 (use_global_workspace=true).
	// When non-nil, Context is empty — the two fields are mutually exclusive.
	// Use assemble_context() in the Python SDK to consume this field.
	WorkingMemory []ContextRef
}

// HandoffKeyWinnerAgentID and HandoffKeyWinnerSourceHash are the response
// Handoff.Context keys on which the Dispatcher stamps the DEFINITION it actually
// dispatched (ADR-0139 D6). The step TaskEvent needs the winner's SourceHash: the
// Gatekeeper and Dispatcher read GetProfile(agent.ID, agent.SourceHash), so an event
// written without it aggregates into `profile:<agent>:`, a row nothing reads
// (T-20261104-routing-never-learns-from-real-outcomes-dag). Kernel-internal and
// server-authoritative like HandoffKeyBudgetOverrun: the Dispatcher overwrites any
// agent-echoed value, and the DAGExecutor consumes both keys so they never merge
// into the plan context. The agent id rides along so the executor uses the hash
// only when the response really came from that winner (a runner-up fallback that
// bypasses the Dispatcher carries no stamp and keeps the empty hash).
//
// HandoffKeyWinnerLatencyMs carries the wall time (ms, decimal) of the winner's
// CallAgent, measured by the Dispatcher around the call (T-20261104-step-taskevents-
// carry-no-latency-so-the-merit). It is the step TaskEvent's latency; the executor
// consumes it on the same terms as the source hash.
const (
	HandoffKeyWinnerAgentID    = "_winner_agent_id"
	HandoffKeyWinnerSourceHash = "_winner_source_hash"
	HandoffKeyWinnerLatencyMs  = "_winner_latency_ms"
)

// DispatchResult bundles the three values returned by StepDispatcher.Execute.
type DispatchResult struct {
	Handoff        *Handoff
	Confidence     float64
	RunnerUps      []ScoredCandidate
	StepAllocation *StepAllocation // populated by StepDispatcher TraitModel sub-selection (ADR-0018); nil-safe
}

// StepDispatcher runs the full auction pipeline: Gatekeeper → ConductAuction → CallAgent.
type StepDispatcher interface {
	Execute(ctx context.Context, task *DispatchTask, in *Handoff) (*DispatchResult, error)
	CallAgent(ctx context.Context, agentID string, handoff *Handoff, excludeInstanceID string) (*Handoff, error)
}

// AgentCaller is the INVOCATION primitive on its own: dispatch a handoff to a
// named agent and return its response. No selection, no bidding.
//
// It exists because most callers never select anything — the privileged organs
// (kg_extractor, docling, reranker, retrieval) already know which agent they
// want, and asking them to depend on the full selection port meant depending on
// a mechanism they do not use. Selection is StepDispatcher; invocation is this.
type AgentCaller interface {
	CallAgent(ctx context.Context, agentID string, handoff *Handoff, excludeInstanceID string) (*Handoff, error)
}

// DispatchTask represents a task/RFP (Request For Proposal) broadcast to agents.
type DispatchTask struct {
	ID              string    `json:"id"`
	Description     string    `json:"description"`
	Context         string    `json:"context"`
	Deadline        time.Time `json:"deadline"`
	RequiredFormats []string  `json:"required_formats,omitempty"`
	// RequiredCapabilities is the ROUTE-03 capability contract carried from the
	// Step into the auction. When non-empty, L1 Declaration hard-gates candidates
	// on required ⊆ manifest.Capabilities. It is populated at the Step→DispatchTask
	// boundary ONLY when the capability_contract arm is on, so an empty slice is
	// the byte-identical control-arm behavior.
	RequiredCapabilities []string `json:"required_capabilities,omitempty"`

	// PreferredAgent / AgentPin carry the step's agent pin into selection (see the
	// PinSoft/PinHard docs). Empty PreferredAgent ⇒ ordinary selection, so the
	// unpinned path is unchanged.
	PreferredAgent string `json:"preferred_agent,omitempty"`
	AgentPin       string `json:"agent_pin,omitempty"`

	// RequiredEffects is the step's effect contract (Step.RequiredEffects), carried
	// unconditionally: it is a fail-closed eligibility gate, not an A/B arm. nil ⇒
	// the step declared none (legacy plan) and dispatch logs it unverified.
	RequiredEffects []string `json:"required_effects,omitempty"`
	// EffectCheck is a diagnostic OUTPUT written by Gatekeeper.FindCandidates (like
	// Funnel): the per-step effect-check record. Nil when the gatekeeper ran none.
	EffectCheck *EffectCheckResult `json:"-"`

	// MaxEnergy and CheckpointAfter are carried from the Step so capability-typed
	// dispatch can apply its per-step policy (ADR-0100 D4): a CHEAP step whose
	// output will be VERIFIED can afford the cheapest competent agent, because the
	// checkpoint catches a bad answer; anything else takes merit-argmax. Both are
	// ignored by the auction, so populating them is inert on that arm.
	MaxEnergy       float64 `json:"max_energy,omitempty"`
	CheckpointAfter bool    `json:"checkpoint_after,omitempty"`

	// Funnel is a ROUTE-02 diagnostic OUTPUT written by Gatekeeper.FindCandidates
	// (not part of the RFP broadcast, hence json:"-"). The StepDispatcher reads it
	// back off the same task pointer when emitting the SelectionEventPayload so the
	// candidate funnel travels with the auction result. Nil when routing tracing
	// is disabled.
	Funnel *GatekeeperFunnel `json:"-"`

	// Subgoal marks a yielded sub-goal (internal/metabolism/yield): dispatched on
	// its parent step's lease, so on a composed lease it would run under the
	// parent's composition. The composer refuses one whose capability hint that
	// composition does not cover (CapabilityMissingError) instead of handing it to
	// a worker with the same tools (T-20261112-yield-sub-goal-on-a-composed-lease-runs-under).
	Subgoal bool `json:"-"`
	// CapabilityResolution is a diagnostic OUTPUT written by
	// Gatekeeper.FindCandidates when it resolved RequiredCapabilities against the
	// live vocabulary (ADR-0100 D5); nil when it ran no resolution.
	CapabilityResolution *CapabilityResolution `json:"-"`
}
