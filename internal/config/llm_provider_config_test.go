package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validProvider is a minimal valid llm_provider config for mutation in tests.
func validProvider() (LLMProviderConfig, EmbedderConfig) {
	p := LLMProviderConfig{
		Default: "qwen-local",
		Generators: []GeneratorConfig{
			{ID: "qwen-local", Provider: "ollama", Model: "qwen3:8b", Endpoint: "http://localhost:11434"},
			{ID: "deepseek", Provider: "openai", Model: "deepseek-v4-flash", Endpoint: "https://x/v1", APIKeyEnv: "K"},
		},
		Roles: map[string]string{"router": "qwen-local", "planner": "deepseek"},
	}
	e := EmbedderConfig{Provider: "ollama", Model: "nomic-embed-text", Dimensions: 768}
	return p, e
}

func TestLLMProviderConfig_Validate_Valid(t *testing.T) {
	p, e := validProvider()
	if errs := p.validate(e); len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}
}

func TestLLMProviderConfig_Validate_Unconfigured_IsNoOp(t *testing.T) {
	// No generators => additive phase => validation skipped even with a bad default.
	p := LLMProviderConfig{Default: "nope"}
	if errs := p.validate(EmbedderConfig{}); errs != nil {
		t.Fatalf("unconfigured provider must skip validation, got %v", errs)
	}
}

func TestLLMProviderConfig_Validate_Errors(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*LLMProviderConfig, *EmbedderConfig)
		want   string
	}{
		{"duplicate id", func(p *LLMProviderConfig, _ *EmbedderConfig) {
			p.Generators[1].ID = "qwen-local"
		}, "duplicated"},
		{"default not a generator", func(p *LLMProviderConfig, _ *EmbedderConfig) {
			p.Default = "ghost"
		}, "default \"ghost\" is not a declared generator"},
		{"role not a generator", func(p *LLMProviderConfig, _ *EmbedderConfig) {
			p.Roles["verifier"] = "ghost"
		}, "is not a declared generator id"},
		{"non-ollama missing api_key_env", func(p *LLMProviderConfig, _ *EmbedderConfig) {
			p.Generators[1].APIKeyEnv = ""
		}, "api_key_env is required for non-ollama"},
		{"missing embedder", func(_ *LLMProviderConfig, e *EmbedderConfig) {
			e.Model = ""
		}, "embedder.model is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, e := validProvider()
			tc.mutate(&p, &e)
			errs := p.validate(e)
			joined := strings.Join(errs, "; ")
			if !strings.Contains(joined, tc.want) {
				t.Errorf("want error containing %q, got %q", tc.want, joined)
			}
		})
	}
}

const baseLLMProviderJSON = `{
	"database": {"host":"localhost","port":"5432","user":"u","password":"p","dbname":"d"},
	"embedder": {"provider":"ollama","model":"nomic-embed-text","endpoint":"http://localhost:11434","dimensions":768},
	"llm_provider": {
		"default": "qwen-local",
		"generators": [
			{"id":"qwen-local","provider":"ollama","model":"qwen3:8b","endpoint":"http://localhost:11434"},
			{"id":"deepseek","provider":"openai","model":"deepseek-v4-flash","endpoint":"https://x/v1","api_key_env":"OPENCODE_API_KEY","cost_per_1m_input":0.0015,"cost_per_1m_output":0.002}
		],
		"roles": {"router":"qwen-local","planner":"deepseek"}
	}
}`

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadConfig_LLMProvider_RoundTripAndDefaults(t *testing.T) {
	cfg, err := LoadConfig(writeTemp(t, baseLLMProviderJSON))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Embedder.Dimensions != 768 || cfg.Embedder.Model != "nomic-embed-text" {
		t.Errorf("embedder not parsed: %+v", cfg.Embedder)
	}
	if len(cfg.LLMProvider.Generators) != 2 {
		t.Fatalf("generators: want 2, got %d", len(cfg.LLMProvider.Generators))
	}
	if cfg.LLMProvider.Default != "qwen-local" {
		t.Errorf("default: want qwen-local, got %q", cfg.LLMProvider.Default)
	}
	if cfg.LLMProvider.Roles["planner"] != "deepseek" {
		t.Errorf("roles.planner: want deepseek, got %q", cfg.LLMProvider.Roles["planner"])
	}
	// Defaults applied.
	for _, g := range cfg.LLMProvider.Generators {
		if g.TimeoutMs != 60000 {
			t.Errorf("generator %q timeout default: want 60000, got %d", g.ID, g.TimeoutMs)
		}
	}
	if cfg.LLMProvider.Health.FailureThreshold != 3 {
		t.Errorf("health.failure_threshold default: want 3, got %d", cfg.LLMProvider.Health.FailureThreshold)
	}
	if cfg.LLMProvider.Health.CooldownMs != 30000 {
		t.Errorf("health.cooldown_ms default: want 30000, got %d", cfg.LLMProvider.Health.CooldownMs)
	}
}

func TestLoadConfig_LLMProvider_ValidationErrorSurfaces(t *testing.T) {
	bad := strings.Replace(baseLLMProviderJSON, `"default": "qwen-local"`, `"default": "ghost"`, 1)
	if _, err := LoadConfig(writeTemp(t, bad)); err == nil {
		t.Fatal("expected LoadConfig to reject a default not in generators")
	} else if !strings.Contains(err.Error(), "not a declared generator") {
		t.Errorf("unexpected error: %v", err)
	}
}
