package llm

import (
	"fmt"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// Client is what NewClient returns: every port a provider adapter implements. One
// value serves the Provider's Acquire path and — through Provider.GenerateOn /
// StreamOn since ADR-0139 wave 1c — the agent-plane gateway, so the two can never be
// built from diverging copies of the generator config again.
type Client interface {
	domain.Generator
	domain.ToolCallingGenerator
	domain.LLMStreamer
	domain.RequestGenerator
}

// NewClient is the ONE constructor of a provider client from a generator's config
// (ADR-0139 D1). It replaces three field-by-field copies — the factory over
// config.ModelConfig, NewProviderRegistry, and NewStreamersFromGenerators (whose own
// comment called itself "the THIRD field-by-field copy"; both deleted in wave 1c, when
// the gateway moved onto the Provider) — each of which had in turn
// missed a field (the credential id on the streaming path, native_tools), so a
// capability silently never arrived. A field added to GeneratorConfig is threaded
// here, once.
func NewClient(g config.GeneratorConfig) (Client, error) {
	with := func(declared domain.Capabilities) *domain.Capabilities {
		if len(g.RequestCapabilities) == 0 {
			return nil
		}
		c := g.ApplyRequestCapabilities(declared)
		return &c
	}
	switch g.Provider {
	case "ollama":
		c := &OllamaClient{BaseURL: g.Endpoint, Model: g.Model, TimeoutMs: g.TimeoutMs,
			NativeTools: g.NativeTools, MaxTokens: g.MaxTokens}
		c.Caps = with(c.Capabilities())
		return c, nil
	case "openai":
		c := &OpenAIClient{
			Endpoint:        g.Endpoint,
			Model:           g.Model,
			APIKeyEnv:       g.APIKeyEnv,
			GeneratorID:     g.ID,
			TimeoutMs:       g.TimeoutMs,
			DisableThinking: g.DisableThinking,
			NativeTools:     g.NativeTools,
			MaxTokens:       g.MaxTokens,
		}
		c.Caps = with(c.Capabilities())
		return c, nil
	case "anthropic":
		c := &AnthropicClient{
			Endpoint:    g.Endpoint,
			Model:       g.Model,
			APIKeyEnv:   g.APIKeyEnv,
			GeneratorID: g.ID,
			TimeoutMs:   g.TimeoutMs,
			MaxTokens:   g.MaxTokens,
			NativeTools: g.NativeTools,
		}
		c.Caps = with(c.Capabilities())
		return c, nil
	case "gemini":
		c := &GeminiClient{
			Endpoint:    g.Endpoint,
			Model:       g.Model,
			APIKeyEnv:   g.APIKeyEnv,
			GeneratorID: g.ID,
			TimeoutMs:   g.TimeoutMs,
			MaxTokens:   g.MaxTokens,
			NativeTools: g.NativeTools,
		}
		c.Caps = with(c.Capabilities())
		return c, nil
	default:
		return nil, fmt.Errorf("unknown LLM provider: %q", g.Provider)
	}
}
