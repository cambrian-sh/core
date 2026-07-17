package app

import (
	"context"
	"slices"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// fakeModelReg is a minimal in-memory registry satisfying modelReconciler. Its
// DeleteAgent builds a fresh slice (never reuses the backing array) so the slice
// reconcileModelAgents is iterating — captured once from GetAllAgents — is not
// mutated mid-loop.
type fakeModelReg struct {
	agents  []domain.AgentDefinition
	deleted []string
}

func (f *fakeModelReg) GetAllAgents(_ context.Context) ([]domain.AgentDefinition, error) {
	return f.agents, nil
}

func (f *fakeModelReg) DeleteAgent(id string) error {
	f.deleted = append(f.deleted, id)
	var out []domain.AgentDefinition
	for _, a := range f.agents {
		if a.ID != id {
			out = append(out, a)
		}
	}
	f.agents = out
	return nil
}

func contains(ss []string, s string) bool { return slices.Contains(ss, s) }

// The reported bug: a model dropped from config still wins the auction after a
// restart because registration is upsert-only. reconcileModelAgents must evict
// the orphan (legacy id scheme included) while leaving the still-declared model
// and every non-model agent untouched.
func TestReconcileModelAgents_PrunesOrphanModelOnly(t *testing.T) {
	reg := &fakeModelReg{agents: []domain.AgentDefinition{
		{ID: "llm:deepseek", Trait: domain.TraitModel},                              // still in config
		{ID: "llm:ollama:qwen3:8b", Trait: domain.TraitModel},                       // orphan (legacy id scheme)
		{ID: "llm:qwen3-local", Trait: domain.TraitModel},                           // orphan (old generator id)
		{ID: "analyst_agent", Trait: domain.TraitCognitive},                         // filesystem agent — must survive
		{ID: "net_agent", Trait: domain.TraitCognitive, Runtime: domain.RuntimeA2A}, // dynamic — must survive
	}}

	reconcileModelAgents(context.Background(), reg, []config.GeneratorConfig{{ID: "deepseek"}})

	if !contains(reg.deleted, "llm:ollama:qwen3:8b") || !contains(reg.deleted, "llm:qwen3-local") {
		t.Fatalf("expected both orphan models pruned, got deleted=%v", reg.deleted)
	}
	if contains(reg.deleted, "llm:deepseek") {
		t.Errorf("still-declared model llm:deepseek was wrongly pruned")
	}
	if contains(reg.deleted, "analyst_agent") || contains(reg.deleted, "net_agent") {
		t.Errorf("non-model agent wrongly pruned: deleted=%v", reg.deleted)
	}
	if len(reg.deleted) != 2 {
		t.Errorf("expected exactly 2 prunes, got %d (%v)", len(reg.deleted), reg.deleted)
	}
}

// With no declared generators every model is an orphan, but non-model agents are
// never touched (a misconfigured/empty llm_provider must not wipe real agents).
func TestReconcileModelAgents_EmptyConfigPrunesAllModelsSparesAgents(t *testing.T) {
	reg := &fakeModelReg{agents: []domain.AgentDefinition{
		{ID: "llm:deepseek", Trait: domain.TraitModel},
		{ID: "summariser_agent", Trait: domain.TraitCognitive},
	}}

	reconcileModelAgents(context.Background(), reg, nil)

	if !contains(reg.deleted, "llm:deepseek") {
		t.Errorf("expected orphan model pruned with empty config, got %v", reg.deleted)
	}
	if contains(reg.deleted, "summariser_agent") {
		t.Errorf("cognitive agent wrongly pruned on empty model config")
	}
}

// Reconcile is idempotent: when config matches the registry exactly, nothing is
// pruned (a healthy boot must be a no-op).
func TestReconcileModelAgents_NoOpWhenConfigMatches(t *testing.T) {
	reg := &fakeModelReg{agents: []domain.AgentDefinition{
		{ID: "llm:deepseek", Trait: domain.TraitModel},
	}}

	reconcileModelAgents(context.Background(), reg, []config.GeneratorConfig{{ID: "deepseek"}})

	if len(reg.deleted) != 0 {
		t.Errorf("expected no prunes when config matches, got %v", reg.deleted)
	}
}
