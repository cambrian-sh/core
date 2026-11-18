package config

import (
	"strings"
	"testing"
)

// T-20261112-pre-send-context-window-check-per-model-with: context_window_tokens
// per generator and llm_provider.context_margin_ratio.

func TestLoadConfig_ContextWindowAndMarginParse(t *testing.T) {
	body := strings.Replace(baseLLMProviderJSON,
		`"endpoint":"https://x/v1",`, `"endpoint":"https://x/v1","context_window_tokens":200000,"max_tokens":8192,`, 1)
	body = strings.Replace(body, `"default": "qwen-local",`, `"default": "qwen-local", "context_margin_ratio": 0.1,`, 1)
	cfg, err := LoadConfig(writeTemp(t, body))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	var got GeneratorConfig
	for _, g := range cfg.LLMProvider.Generators {
		if g.ID == "deepseek" {
			got = g
		}
	}
	if got.ContextWindowTokens != 200000 || got.MaxTokens != 8192 {
		t.Fatalf("deepseek = %+v, want context_window_tokens 200000, max_tokens 8192", got)
	}
	if m := cfg.LLMProvider.ContextMargin(); m != 0.1 {
		t.Fatalf("ContextMargin = %v, want 0.1", m)
	}
}

// An existing file without the new keys loads unchanged: no window (no check)
// and the default margin. That is why the fields need no schema-version bump.
func TestLoadConfig_ContextWindowAbsentMeansNoCheck(t *testing.T) {
	cfg, err := LoadConfig(writeTemp(t, baseLLMProviderJSON))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	for _, g := range cfg.LLMProvider.Generators {
		if g.ContextWindowTokens != 0 {
			t.Fatalf("generator %q: context window %d without the key, want 0", g.ID, g.ContextWindowTokens)
		}
	}
	if m := cfg.LLMProvider.ContextMargin(); m != DefaultContextMarginRatio {
		t.Fatalf("ContextMargin = %v, want the default %v", m, DefaultContextMarginRatio)
	}
}

func TestLLMProviderConfig_Validate_ContextWindow(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*LLMProviderConfig)
		want   string // "" = valid
	}{
		{"window set", func(p *LLMProviderConfig) { p.Generators[1].ContextWindowTokens = 128000 }, ""},
		{"negative window", func(p *LLMProviderConfig) { p.Generators[1].ContextWindowTokens = -1 }, "context_window_tokens must be >= 0"},
		{"output cap fills the window", func(p *LLMProviderConfig) {
			p.Generators[1].ContextWindowTokens = 4096
			p.Generators[1].MaxTokens = 4096
		}, "must be below context_window_tokens"},
		{"negative margin", func(p *LLMProviderConfig) { p.ContextMarginRatio = -0.1 }, "context_margin_ratio"},
		{"margin of one", func(p *LLMProviderConfig) { p.ContextMarginRatio = 1 }, "context_margin_ratio"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, e := validProvider()
			tc.mutate(&p)
			joined := strings.Join(p.validate(e), "; ")
			if tc.want == "" && joined != "" {
				t.Fatalf("want valid, got %q", joined)
			}
			if tc.want != "" && !strings.Contains(joined, tc.want) {
				t.Fatalf("want error containing %q, got %q", tc.want, joined)
			}
		})
	}
}
