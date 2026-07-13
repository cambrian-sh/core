package kernel

import (
	"context"

	"github.com/cambrian-sh/core/internal/awareness"
	"github.com/cambrian-sh/core/domain"
)


// AwarenessStack is the LLM + planning layer. It owns the Planner which
// generates ExecutionPlans and synthesises reasoning output.
//
// Biologically: this is the prefrontal cortex — zero-hardcode routing.
type AwarenessStack struct {
	LLM     domain.Generator
	Planner *awareness.Planner
}

// NewAwarenessStack constructs the planning layer.
// The planner needs the AgentRegistry (as AgentProvider) and a ProceduralMemory
// (from MemoryStack) for procedural memory injection.
// WorkspaceStage (ADR-0016) enriches the Planner with cross-session LTM facts.
// policyProvider (ADR-0027) validates the LLM-emitted cache_policy; nil = skip validation.
func NewAwarenessStack(
	llm domain.Generator,
	registry domain.AgentRegistry,
	hippocampus domain.ProceduralMemory,
	workspaceStage domain.WorkspaceStage,
	policyProvider ...domain.PolicyProvider,
) *AwarenessStack {
	planner := awareness.NewPlanner(llm, registry, hippocampus)
	planner.WorkspaceStage = workspaceStage
	if len(policyProvider) > 0 && policyProvider[0] != nil {
		planner.SetPolicyProvider(policyProvider[0])
	}
	return &AwarenessStack{
		LLM:     llm,
		Planner: planner,
	}
}

// Start is a no-op — the AwarenessStack has no background workers.
func (s *AwarenessStack) Start(_ context.Context) error {
	return nil
}

// Shutdown is a no-op.
func (s *AwarenessStack) Shutdown(_ context.Context) {}
