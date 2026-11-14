package domain

import "context"

// Purpose identifies why an organ needs a generator. System-organ purposes
// (router/planner/verifier/memory/facts) resolve via deterministic role
// config; PurposeAgentStep takes no role preference — agent-step model choice
// is the auction's StepAllocation (ADR-0018), served by the LLMGateway. This
// split is what keeps the Provider's availability authority Zero-Hardcode-legal
// (ADR-0042 D1).
type Purpose string

const (
	PurposeRouter   Purpose = "router"
	PurposePlanner  Purpose = "planner"
	PurposeVerifier Purpose = "verifier"
	// PurposeInterview ("interview") was RETIRED 2026-11-05 with the LLM agent
	// interview, its only consumer (T-20261105-delete-the-llm-agent-interview-now-owner). A
	// config whose llm_provider.roles still names it loads; the role is dropped
	// with a log line (llm.RetiredRoles).
	PurposeMemory Purpose = "memory"
	// PurposeFacts is the ADR-0136 D4/D5 fact extractor (one call per ingested
	// document, plus at most one supersession call). Its own role, so an operator can
	// route this high-volume organ to a cheap generator (ADR-0131 D5: cost routing
	// lives in roles) without moving the memory organ.
	PurposeFacts Purpose = "facts"
	// PurposeThought is an in-kernel thought step (ADR-0139 C1/D2): the kernel
	// answering a plan step itself from its upstream results. It is a DELIVERABLE,
	// like an agent step's reply, not a plan — so a hook scoped to deliverables is
	// purposes [agent_step, thought], and one scoped to planner no longer rewrites
	// it. Routing is unchanged by default: with no `thought` role configured it
	// resolves to whatever `planner` resolves to (llm.RoleFallbacks).
	// T-20261106-thought-steps-call-the-model-under-the-planner.
	PurposeThought Purpose = "thought"
	// PurposeRetrievalPlanner is the agentic retrieval planner (the retrieval_agent's
	// plan/decompose/decide calls on the recall path). With no `retrieval_planner`
	// role configured it resolves to whatever `memory` resolves to
	// (llm.RoleFallbacks), so it follows role switches; an explicit
	// execution.retrieval.agentic_planner_model generator id still overrides
	// (owner decision 2026-11-14, third round;
	// T-20261102-agentic-planner-model-names-a-generator-id).
	PurposeRetrievalPlanner Purpose = "retrieval_planner"
	PurposeAgentStep        Purpose = "agent_step"
)

// LLMRequest is what an organ states when it needs an LLM. The organ declares
// its need and may suggest a model; the Provider makes the final, health-guarded
// decision (ADR-0042 D3). SuggestedModelID is a prior, never a command.
type LLMRequest struct {
	Purpose          Purpose
	CapabilityHints  []string
	SuggestedModelID string
}

// LLMProvider is the sole authority on LLM availability and provisioning. It
// returns a Generator already resolved to a healthy model and instrumented to
// record its own health and cost. Organs stay blind to model identity and
// health (ADR-0042).
type LLMProvider interface {
	Acquire(ctx context.Context, req LLMRequest) (Generator, error)
}
