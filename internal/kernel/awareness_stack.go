package kernel

import (
	"context"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/awareness"
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
// The planner needs the AgentRegistry (as AgentProvider).
// WorkspaceStage (ADR-0016) enriches the Planner with cross-session LTM facts.
// (The ADR-0027 procedural memory and cache_policy provider were removed
// 2026-11-14, ENG-1301.)
func NewAwarenessStack(
	llm domain.Generator,
	registry domain.AgentRegistry,
	workspaceStage domain.WorkspaceStage,
) *AwarenessStack {
	planner := awareness.NewPlanner(llm, registry)
	planner.WorkspaceStage = workspaceStage
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
