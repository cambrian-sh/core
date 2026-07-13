package llm

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/domain"
)

func testProvider(t *testing.T) *Provider {
	t.Helper()
	p, err := NewProvider(config.LLMProviderConfig{
		Default: "deepseek",
		Generators: []config.GeneratorConfig{
			{ID: "qwen-local", Provider: "ollama", Model: "qwen3:8b", Endpoint: "http://localhost:11434"},
			{ID: "deepseek", Provider: "openai", Model: "deepseek-v4-flash", Endpoint: "https://x/v1", APIKeyEnv: "K"},
		},
		Roles:  map[string]string{"router": "qwen-local", "planner": "deepseek"},
		Health: config.HealthConfig{FailureThreshold: 1, CooldownMs: 60000},
	}, nil)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return p
}

func TestProvider_RoleResolvesToConfiguredID(t *testing.T) {
	p := testProvider(t)
	id, err := p.resolve(context.Background(), domain.LLMRequest{Purpose: domain.PurposeRouter})
	if err != nil || id != "qwen-local" {
		t.Fatalf("router role: want qwen-local, got %q (%v)", id, err)
	}
}

func TestProvider_AgentStepUsesHealthySuggestion(t *testing.T) {
	p := testProvider(t)
	id, err := p.resolve(context.Background(), domain.LLMRequest{Purpose: domain.PurposeAgentStep, SuggestedModelID: "deepseek"})
	if err != nil || id != "deepseek" {
		t.Fatalf("agent step suggestion: want deepseek, got %q (%v)", id, err)
	}
}

func TestProvider_FailsOverWhenRoleModelUnhealthy(t *testing.T) {
	p := testProvider(t)
	p.breaker.Record("qwen-local", false) // threshold 1 => qwen-local OPEN
	// router role prefers qwen-local (open) -> default deepseek (healthy).
	id, err := p.resolve(context.Background(), domain.LLMRequest{Purpose: domain.PurposeRouter})
	if err != nil || id != "deepseek" {
		t.Fatalf("failover: want deepseek (default), got %q (%v)", id, err)
	}
}

func TestProvider_NoHealthyModelReturnsError(t *testing.T) {
	p := testProvider(t)
	p.breaker.Record("qwen-local", false)
	p.breaker.Record("deepseek", false)
	if _, err := p.resolve(context.Background(), domain.LLMRequest{Purpose: domain.PurposeRouter}); !errors.Is(err, ErrNoHealthyModel) {
		t.Fatalf("want ErrNoHealthyModel, got %v", err)
	}
}

func TestProvider_AcquireReturnsGeneratorForHealthyRequest(t *testing.T) {
	p := testProvider(t)
	gen, err := p.Acquire(context.Background(), domain.LLMRequest{Purpose: domain.PurposePlanner})
	if err != nil || gen == nil {
		t.Fatalf("Acquire planner: want generator, got %v (%v)", gen, err)
	}
}

func TestProvider_AcquirePropagatesNoHealthyError(t *testing.T) {
	p := testProvider(t)
	p.breaker.Record("qwen-local", false)
	p.breaker.Record("deepseek", false)
	if _, err := p.Acquire(context.Background(), domain.LLMRequest{Purpose: domain.PurposeMemory}); !errors.Is(err, ErrNoHealthyModel) {
		t.Fatalf("Acquire should propagate ErrNoHealthyModel, got %v", err)
	}
}

func TestProvider_TraceWrapperAppliedPerPurpose(t *testing.T) {
	p := testProvider(t)
	var gotSubsystem string
	wrapped := false
	p.SetTraceWrapper(func(g domain.Generator, subsystem string) domain.Generator {
		gotSubsystem = subsystem
		wrapped = true
		return g
	})
	// Every Acquire — including the router purpose — must flow through the trace
	// wrapper, labelled by purpose. This is the regression guard for the router
	// tracing gap that the per-organ wrapping missed.
	if _, err := p.Acquire(context.Background(), domain.LLMRequest{Purpose: domain.PurposeRouter}); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if !wrapped || gotSubsystem != "router" {
		t.Fatalf("trace wrapper not applied per purpose: wrapped=%v subsystem=%q", wrapped, gotSubsystem)
	}
}

func TestProvider_AgentStepPreferenceHookUsed(t *testing.T) {
	p := testProvider(t)
	p.breaker.Record("deepseek", false) // suggested open
	p.SetAgentStepPreference(func(_ context.Context, _ domain.LLMRequest) []string {
		return []string{"qwen-local"} // EFE prefers qwen-local
	})
	id, err := p.resolve(context.Background(), domain.LLMRequest{Purpose: domain.PurposeAgentStep, SuggestedModelID: "deepseek"})
	if err != nil || id != "qwen-local" {
		t.Fatalf("EFE preference: want qwen-local, got %q (%v)", id, err)
	}
}
