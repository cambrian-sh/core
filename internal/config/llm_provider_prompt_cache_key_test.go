package config

import (
	"strings"
	"testing"
)

// ENG-2133: generators[].prompt_cache_key — opt-in, off by default, OpenAI provider only.

func TestLoadConfig_PromptCacheKeyParsesAndDefaultsOff(t *testing.T) {
	body := strings.Replace(baseLLMProviderJSON,
		`"endpoint":"https://x/v1",`, `"endpoint":"https://x/v1","prompt_cache_key":true,`, 1)
	cfg, err := LoadConfig(writeTemp(t, body))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	for _, g := range cfg.LLMProvider.Generators {
		if want := g.ID == "deepseek"; g.PromptCacheKey != want {
			t.Errorf("%s: prompt_cache_key = %v, want %v (absent means off)", g.ID, g.PromptCacheKey, want)
		}
	}
}

// On a provider that does not read it the switch is refused rather than silently ignored.
func TestLoadConfig_PromptCacheKeyRefusedOnOtherProviders(t *testing.T) {
	body := strings.Replace(baseLLMProviderJSON,
		`"endpoint":"http://localhost:11434"},`, `"endpoint":"http://localhost:11434","prompt_cache_key":true},`, 1)
	if _, err := LoadConfig(writeTemp(t, body)); err == nil || !strings.Contains(err.Error(), "prompt_cache_key") {
		t.Fatalf("an ollama generator with prompt_cache_key loaded: err = %v", err)
	}
}
