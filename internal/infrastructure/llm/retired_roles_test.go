package llm

import (
	"testing"

	"github.com/cambrian-sh/core/internal/config"
)

// A config that still binds the retired `interview` role (2026-11-05, the LLM agent
// interview was deleted) must keep loading: the Provider drops the binding instead
// of failing, and live roles in the same map are kept.
func TestNewProvider_DropsRetiredInterviewRole(t *testing.T) {
	p, err := NewProvider(config.LLMProviderConfig{
		Default: "g",
		Generators: []config.GeneratorConfig{{
			ID: "g", Provider: "openai", Model: "m", Endpoint: "http://127.0.0.1:1", APIKeyEnv: "RETIRED_ROLE_UNSET_KEY", TimeoutMs: 1000,
		}},
		Roles:  map[string]string{"interview": "g", "planner": "g"},
		Health: config.HealthConfig{FailureThreshold: 3, CooldownMs: 1000},
	}, nil)
	if err != nil {
		t.Fatalf("NewProvider rejected a config naming the retired interview role: %v", err)
	}
	roles := p.Roles()
	if _, ok := roles["interview"]; ok {
		t.Errorf("retired interview role still bound: %v", roles)
	}
	if roles["planner"] != "g" {
		t.Errorf("live planner role lost: %v", roles)
	}
}
