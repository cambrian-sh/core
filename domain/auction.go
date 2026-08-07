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
}
