package domain

import "context"

// Purpose identifies why an organ needs a generator. System-organ purposes
// (router/planner/verifier/interview/memory) resolve via deterministic role
// config; PurposeAgentStep takes no role preference — agent-step model choice
// is the auction's StepAllocation (ADR-0018), served by the LLMGateway. This
// split is what keeps the Provider's availability authority Zero-Hardcode-legal
// (ADR-0042 D1).
type Purpose string

const (
	PurposeRouter    Purpose = "router"
	PurposePlanner   Purpose = "planner"
	PurposeVerifier  Purpose = "verifier"
	PurposeInterview Purpose = "interview"
	PurposeMemory    Purpose = "memory"
	// PurposeFacts is the ADR-0136 D4/D5 fact extractor (one call per ingested
	// document, plus at most one supersession call). Its own role, so an operator can
	// route this high-volume organ to a cheap generator (ADR-0131 D5: cost routing
	// lives in roles) without moving the memory organ.
	PurposeFacts     Purpose = "facts"
	PurposeAgentStep Purpose = "agent_step"
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
