package llm

import (
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// Owner decision 2026-11-14, third round (T-20261102-agentic-planner-model-names-a-generator-id):
// the retrieval_planner purpose follows the memory role until an operator binds a
// retrieval_planner role, and a live role switch moves it on the next lookup.
func TestRoleGenerator_RetrievalPlannerFallsBackToMemoryAndIsLive(t *testing.T) {
	t.Parallel()
	p, err := NewProvider(config.LLMProviderConfig{
		Default: "d",
		Generators: []config.GeneratorConfig{
			{ID: "d", Provider: "openai", Model: "m-d", Endpoint: "http://127.0.0.1:1", TimeoutMs: 1000},
			{ID: "mem", Provider: "openai", Model: "m-mem", Endpoint: "http://127.0.0.1:1", TimeoutMs: 1000},
			{ID: "rp", Provider: "openai", Model: "m-rp", Endpoint: "http://127.0.0.1:1", TimeoutMs: 1000},
		},
		Roles:  map[string]string{"memory": "mem"},
		Health: config.HealthConfig{FailureThreshold: 5, CooldownMs: 60000},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.RoleGenerator(domain.PurposeRetrievalPlanner); got != "mem" {
		t.Fatalf("unbound retrieval_planner resolved to %q, want the memory role's mem", got)
	}
	if _, listed := p.Roles()["retrieval_planner"]; listed {
		t.Fatal("an unbound retrieval_planner purpose must not appear as a configured role")
	}
	p.SetRole("memory", "d")
	if got := p.RoleGenerator(domain.PurposeRetrievalPlanner); got != "d" {
		t.Fatalf("after switching memory, retrieval_planner resolved to %q, want d", got)
	}
	p.SetRole("retrieval_planner", "rp")
	if got := p.RoleGenerator(domain.PurposeRetrievalPlanner); got != "rp" {
		t.Fatalf("a bound retrieval_planner role resolved to %q, want rp", got)
	}
	if got := p.RoleGenerator(domain.PurposeVerifier); got != "" {
		t.Fatalf("an unbound purpose with no fallback resolved to %q, want \"\"", got)
	}
}
