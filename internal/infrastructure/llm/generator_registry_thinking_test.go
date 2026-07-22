package llm_test

import (
	"testing"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
)

// TestGeneratorRegistry_PropagatesDisableThinking guards the ADR-0042 registry
// path (the one NewProvider uses) against silently dropping per-generator config
// fields when constructing clients.
//
// Regression: the registry built its ModelConfig field-by-field and omitted
// DisableThinking, so `"disable_thinking": true` in providers.json was ignored
// on the live path. For a reasoning model (deepseek-v4-flash on opencode) that
// meant every call generated reasoning tokens first — burning the whole
// max_tokens budget and returning EMPTY content at ~45s. Empty output is scored
// as a failure by healthGenerator, so this also drove the circuit breaker open.
func TestGeneratorRegistry_PropagatesDisableThinking(t *testing.T) {
	reg, err := llm.NewGeneratorRegistry([]config.GeneratorConfig{
		{
			ID:              "reasoner",
			Provider:        "openai",
			Model:           "deepseek-v4-flash",
			Endpoint:        "https://example.invalid/v1",
			APIKeyEnv:       "TEST_KEY",
			TimeoutMs:       60000,
			DisableThinking: true,
		},
		{
			ID:        "plain",
			Provider:  "openai",
			Model:     "some-model",
			Endpoint:  "https://example.invalid/v1",
			APIKeyEnv: "TEST_KEY",
			TimeoutMs: 60000,
		},
	})
	if err != nil {
		t.Fatalf("NewGeneratorRegistry: %v", err)
	}

	for _, tc := range []struct {
		id   string
		want bool
	}{{"reasoner", true}, {"plain", false}} {
		entry, ok := reg.Lookup(tc.id)
		if !ok {
			t.Fatalf("generator %q not in registry", tc.id)
		}
		client, ok := entry.Generator.(*llm.OpenAIClient)
		if !ok {
			t.Fatalf("generator %q: want *OpenAIClient, got %T", tc.id, entry.Generator)
		}
		if client.DisableThinking != tc.want {
			t.Errorf("generator %q: DisableThinking = %v, want %v",
				tc.id, client.DisableThinking, tc.want)
		}
	}
}
