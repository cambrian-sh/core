package llm

import (
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/internal/config"
	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// Every configured generator — across providers — gets a streaming client keyed
// by its auction id. The regression being locked: an openai generator (deepseek)
// previously had NO streaming client, so cognitive agents could not generate.
func TestNewStreamersFromGenerators_AllProvidersKeyedByID(t *testing.T) {
	gens := []config.GeneratorConfig{
		{ID: "deepseek", Provider: "openai", Model: "deepseek-v4-flash", Endpoint: "https://x/v1", APIKeyEnv: "K"},
		{ID: "qwen-local", Provider: "ollama", Model: "qwen3:8b", Endpoint: "http://localhost:11434"},
		{ID: "claude", Provider: "anthropic", Model: "claude-x", Endpoint: "https://y", APIKeyEnv: "AK"},
	}

	streamers, err := NewStreamersFromGenerators(gens)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	for _, id := range []string{"llm:deepseek", "llm:qwen-local", "llm:claude"} {
		s, ok := streamers[id]
		if !ok || s == nil {
			t.Fatalf("missing streaming client for %s", id)
		}
		var _ domain.LLMStreamer = s // compile-time: it is a streamer
	}
	if _, ok := streamers["llm:deepseek"].(*OpenAIClient); !ok {
		t.Errorf("llm:deepseek should be an *OpenAIClient streamer (the deepseek-can't-stream bug)")
	}
	if _, ok := streamers["llm:qwen-local"].(*OllamaClient); !ok {
		t.Errorf("llm:qwen-local should be an *OllamaClient streamer")
	}
}

// An unknown provider is skipped (with a reported error) without dropping the
// valid siblings — one bad generator must not disable streaming for the rest.
func TestNewStreamersFromGenerators_UnknownProviderSkippedWithError(t *testing.T) {
	gens := []config.GeneratorConfig{
		{ID: "good", Provider: "ollama", Model: "m", Endpoint: "e"},
		{ID: "bad", Provider: "bogus", Model: "m", Endpoint: "e"},
	}

	streamers, err := NewStreamersFromGenerators(gens)
	if err == nil {
		t.Error("expected an error for the unknown provider")
	}
	if _, ok := streamers["llm:good"]; !ok {
		t.Error("the valid generator should still be registered despite a sibling error")
	}
	if _, ok := streamers["llm:bad"]; ok {
		t.Error("the unknown-provider generator should be skipped")
	}
}
