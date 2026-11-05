package config

import (
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0139 C1: request_capabilities is validated at load (a typo must not be a
// silently accepted no-op) and applied over the adapter's declaration.
func TestGeneratorRequestCapabilities_ValidatedAtLoad(t *testing.T) {
	t.Parallel()
	c := LLMProviderConfig{
		Default: "g",
		Generators: []GeneratorConfig{{
			ID: "g", Provider: "openai", Model: "m",
			RequestCapabilities: map[string]string{"samplng": "unsupported", "stop": "ignored"},
		}},
	}
	errs := c.validate(EmbedderConfig{Model: "e"})
	joined := strings.Join(errs, "\n")
	if !strings.Contains(joined, `unknown key "samplng"`) {
		t.Errorf("a misspelt key must be refused, got %v", errs)
	}
	if !strings.Contains(joined, `request_capabilities.stop: "ignored"`) {
		t.Errorf("an unknown level must be refused, got %v", errs)
	}
}

func TestGeneratorRequestCapabilities_AppliedOverDeclaration(t *testing.T) {
	t.Parallel()
	g := GeneratorConfig{RequestCapabilities: map[string]string{"sampling": "unsupported", "response_schema": "best_effort"}}
	got := g.ApplyRequestCapabilities(domain.Capabilities{Sampling: domain.LevelEnforced, Stop: domain.LevelEnforced, ResponseSchema: domain.LevelEnforced})
	if got.Sampling != domain.LevelUnsupported || got.ResponseSchema != domain.LevelBestEffort {
		t.Errorf("overrides not applied: %+v", got)
	}
	if got.Stop != domain.LevelEnforced {
		t.Errorf("an unoverridden field must keep the declaration: %+v", got)
	}
}

// The Anthropic Messages API requires max_tokens; the default now comes from
// config (it used to be a 4096 literal in the adapter).
func TestApplyGeneratorDefaults_AnthropicMaxTokens(t *testing.T) {
	t.Parallel()
	gens := []GeneratorConfig{
		{ID: "a", Provider: "anthropic"},
		{ID: "b", Provider: "anthropic", MaxTokens: 1000},
		{ID: "o", Provider: "openai"},
	}
	ApplyGeneratorDefaults(gens)
	if gens[0].MaxTokens != AnthropicDefaultMaxTokens {
		t.Errorf("anthropic default = %d, want %d", gens[0].MaxTokens, AnthropicDefaultMaxTokens)
	}
	if gens[1].MaxTokens != 1000 {
		t.Errorf("a configured max_tokens must survive defaults, got %d", gens[1].MaxTokens)
	}
	if gens[2].MaxTokens != 0 {
		t.Errorf("openai sends no cap unless configured, got %d", gens[2].MaxTokens)
	}
}
