package app

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// reconcileFilesystemAgents evicts a filesystem agent whose source file is gone,
// keeps one whose file is present, and — critically — never touches a model or a
// dynamically-registered A2A agent (neither has a reconcilable local source).
func TestReconcileFilesystemAgents_PrunesMissingSourceOnly(t *testing.T) {
	reg := &fakeModelReg{agents: []domain.AgentDefinition{
		{ID: "ghost_agent", Trait: domain.TraitCognitive, Runtime: domain.RuntimePython, ExecPath: "/agents/ghost_agent.py"},
		{ID: "live_agent", Trait: domain.TraitCognitive, Runtime: domain.RuntimePython, ExecPath: "/agents/live_agent.py"},
		{ID: "llm:deepseek", Trait: domain.TraitModel},
		{ID: "net_agent", Trait: domain.TraitCognitive, Runtime: domain.RuntimeA2A, ExecPath: "/agents/stale_a2a.py"},
		{ID: "modelless", Trait: domain.TraitCognitive, ExecPath: ""},
	}}

	// Only live_agent.py exists on disk.
	exists := func(p string) bool { return p == "/agents/live_agent.py" }

	reconcileFilesystemAgents(context.Background(), reg, exists)

	if !contains(reg.deleted, "ghost_agent") {
		t.Errorf("expected ghost_agent (missing source) pruned, got %v", reg.deleted)
	}
	if contains(reg.deleted, "live_agent") {
		t.Errorf("live_agent (source present) wrongly pruned")
	}
	if contains(reg.deleted, "llm:deepseek") {
		t.Errorf("model wrongly pruned by filesystem reconcile")
	}
	if contains(reg.deleted, "net_agent") {
		t.Errorf("A2A agent wrongly pruned despite missing local path — dynamic agents must be spared")
	}
	if contains(reg.deleted, "modelless") {
		t.Errorf("agent without ExecPath wrongly pruned (nothing to reconcile against)")
	}
	if len(reg.deleted) != 1 {
		t.Errorf("expected exactly 1 prune, got %d (%v)", len(reg.deleted), reg.deleted)
	}
}

// When every source file is present, the reconcile is a no-op.
func TestReconcileFilesystemAgents_NoOpWhenAllPresent(t *testing.T) {
	reg := &fakeModelReg{agents: []domain.AgentDefinition{
		{ID: "a", Trait: domain.TraitCognitive, Runtime: domain.RuntimePython, ExecPath: "/agents/a.py"},
		{ID: "b", Trait: domain.TraitCognitive, Runtime: domain.RuntimePython, ExecPath: "/agents/b.py"},
	}}
	reconcileFilesystemAgents(context.Background(), reg, func(string) bool { return true })
	if len(reg.deleted) != 0 {
		t.Errorf("expected no prunes when all sources present, got %v", reg.deleted)
	}
}
