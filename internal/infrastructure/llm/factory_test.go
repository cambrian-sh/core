package llm_test

import (
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
)

// NewClient is the one constructor (ADR-0139 D1): every provider yields a value
// that serves every port, so the Provider path and the streaming gateway can never
// again be built from diverging copies of the generator config.
func TestNewClient_EveryProviderServesEveryPort(t *testing.T) {
	t.Parallel()
	for _, g := range []config.GeneratorConfig{
		{ID: "o", Provider: "ollama", Model: "llama3", Endpoint: "http://localhost:11434", TimeoutMs: 60000},
		{ID: "a", Provider: "openai", Model: "gpt-4o", Endpoint: "https://api.openai.com/v1", APIKeyEnv: "OPENAI_API_KEY", TimeoutMs: 30000},
		{ID: "c", Provider: "anthropic", Model: "claude-sonnet-4-5", Endpoint: "https://api.anthropic.com/v1", TimeoutMs: 30000},
		{ID: "g", Provider: "gemini", Model: "gemini-2.0-flash", Endpoint: "https://generativelanguage.googleapis.com/v1beta", TimeoutMs: 30000},
	} {
		c, err := llm.NewClient(g)
		if err != nil {
			t.Fatalf("NewClient(%s): %v", g.Provider, err)
		}
		var _ domain.Generator = c
		var _ domain.LLMStreamer = c
		if _, ok := domain.AsRequestGenerator(c); !ok {
			t.Errorf("%s: client must be a RequestGenerator", g.Provider)
		}
	}
}

func TestNewClient_UnknownProvider(t *testing.T) {
	t.Parallel()
	if _, err := llm.NewClient(config.GeneratorConfig{Provider: "no-such-provider", Model: "m"}); err == nil {
		t.Fatal("want error for unknown provider, got nil")
	}
}

// The per-generator capability override (config request_capabilities) must reach
// the client (AGENTS.md §3.4: a validated config field is threaded in the same
// change). haiku-local's probe values are the case that motivated it.
func TestNewClient_RequestCapabilitiesOverrideReachesClient(t *testing.T) {
	t.Parallel()
	c, err := llm.NewClient(config.GeneratorConfig{
		ID: "haiku-local", Provider: "openai", Model: "claude-haiku-4-5", Endpoint: "http://127.0.0.1:3131/v1",
		NativeTools: true,
		RequestCapabilities: map[string]string{
			"sampling": "unsupported", "stop": "unsupported", "response_schema": "best_effort",
			"max_tokens": "best_effort", "cache_breakpoints": "unsupported", "images": "unsupported",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	caps := c.Capabilities()
	if caps.Sampling != domain.LevelUnsupported || caps.Stop != domain.LevelUnsupported {
		t.Errorf("sampling/stop override lost: %+v", caps)
	}
	if caps.MaxTokens != domain.LevelBestEffort || caps.ResponseSchema != domain.LevelBestEffort {
		t.Errorf("max_tokens/response_schema override lost: %+v", caps)
	}
	if caps.Tools != domain.LevelEnforced || caps.SystemRole != domain.LevelEnforced {
		t.Errorf("unoverridden fields must keep the adapter's declaration: %+v", caps)
	}
}

func TestCalculateCost(t *testing.T) {
	t.Parallel()
	usage := llm.TokenUsage{PromptTokens: 500000, CompletionTokens: 500000}
	cost := llm.CalculateCost(usage, 5.0, 15.0)
	if cost < 9.9 || cost > 10.1 {
		t.Errorf("want ~10.0 (0.5*5 + 0.5*15), got %v", cost)
	}
}

func TestCalculateCost_ZeroTokens(t *testing.T) {
	t.Parallel()
	cost := llm.CalculateCost(llm.TokenUsage{}, 5.0, 15.0)
	if cost != 0.0 {
		t.Errorf("want 0.0, got %v", cost)
	}
}
