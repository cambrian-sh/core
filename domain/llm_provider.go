package domain

import "context"

// Purpose identifies why an organ needs a generator. System-organ purposes
// (router/planner/verifier/interview/memory) resolve via deterministic role
// config; PurposeAgentStep resolves via the EFE/auction preference layer. This
// split is what keeps the Provider's availability authority Zero-Hardcode-legal
// (ADR-0042 D1).
type Purpose string

const (
	PurposeRouter    Purpose = "router"
	PurposePlanner   Purpose = "planner"
	PurposeVerifier  Purpose = "verifier"
	PurposeInterview Purpose = "interview"
	PurposeMemory    Purpose = "memory"
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
