package llm

import (
	"testing"

	"github.com/cambrian-sh/core/internal/config"
)

// GeneratorConfig is copied field-by-field into ModelConfig and into three
// separate client constructors, and its own comment warns that a field added in
// one place and forgotten in another "silently never arrives".
//
// That is exactly what happened to GeneratorID. It was threaded through the
// generator registry and missed on the streaming path, so `llm:deepseek` — the
// lane the chat agent actually uses — built a client with no id, could not find
// the credential stored against that id, and sent every request unauthenticated.
// The endpoint answered 401 "Missing API key" while the console showed the key
// installed.
//
// This test covers every construction path at once so the next field cannot be
// half-threaded the same way.
func TestGeneratorID_ReachesEveryClientConstruction(t *testing.T) {
	generators := []config.GeneratorConfig{
		{ID: "deepseek", Provider: "openai", Model: "deepseek-v4-flash", Endpoint: "https://x/v1"},
		{ID: "claude", Provider: "anthropic", Model: "claude", Endpoint: "https://y"},
		{ID: "gem", Provider: "gemini", Model: "gemini", Endpoint: "https://z"},
	}

	t.Run("streaming clients", func(t *testing.T) {
		streamers, err := NewStreamersFromGenerators(generators)
		if err != nil {
			t.Fatalf("NewStreamersFromGenerators: %v", err)
		}
		for _, g := range generators {
			s, ok := streamers["llm:"+g.ID]
			if !ok {
				t.Fatalf("no streaming client for %q", g.ID)
			}
			if got := generatorIDOf(s); got != g.ID {
				t.Errorf("%s streaming client carries id %q, want %q", g.Provider, got, g.ID)
			}
		}
	})

	t.Run("factory", func(t *testing.T) {
		for _, g := range generators {
			gen, _, err := NewClient(config.ModelConfig{
				ID: g.ID, Provider: g.Provider, Model: g.Model, Endpoint: g.Endpoint,
			})
			if err != nil {
				t.Fatalf("NewClient(%s): %v", g.Provider, err)
			}
			if got := generatorIDOf(gen); got != g.ID {
				t.Errorf("%s client carries id %q, want %q", g.Provider, got, g.ID)
			}
		}
	})

	t.Run("generator registry", func(t *testing.T) {
		reg, err := NewGeneratorRegistry(generators)
		if err != nil {
			t.Fatalf("NewGeneratorRegistry: %v", err)
		}
		for _, g := range generators {
			e, ok := reg.entries[g.ID]
			if !ok {
				t.Fatalf("no entry for %q", g.ID)
			}
			if got := generatorIDOf(e.Generator); got != g.ID {
				t.Errorf("%s registry client carries id %q, want %q", g.Provider, got, g.ID)
			}
		}
	})
}

// generatorIDOf reads the id back off whichever client type this is.
func generatorIDOf(v any) string {
	switch c := v.(type) {
	case *OpenAIClient:
		return c.GeneratorID
	case *AnthropicClient:
		return c.GeneratorID
	case *GeminiClient:
		return c.GeneratorID
	}
	return "<not a keyed client>"
}
