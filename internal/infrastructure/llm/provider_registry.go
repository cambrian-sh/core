package llm

import (
	"fmt"

	"github.com/cambrian-sh/cambrian-runtime/internal/config"
	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// ProviderRegistry holds initialized LLM provider clients and extractors,
// keyed by provider name. It is the single point from which all LLM routing
// originates, forcing the knowledge graph to cluster all providers into
// one community rather than fragmented islands.
type ProviderRegistry struct {
	Ollama    *OllamaClient
	OpenAI    *OpenAIClient
	Anthropic *AnthropicClient
	Gemini    *GeminiClient

	OllamaEmbedder *OllamaEmbedder
}

// clientEntry binds a Generator to its TokenUsageExtractor for cost tracking.
type clientEntry struct {
	Generator domain.Generator
	Extractor TokenUsageExtractor
}

// NewProviderRegistryFromGenerators builds a ProviderRegistry from ADR-0042
// generator config. The new Provider owns the non-streaming Acquire path; this
// registry is retained only as the streaming-client source for the ADR-0018
// gateway (streaming is out of ADR-0042 scope), built from the same generators.
func NewProviderRegistryFromGenerators(generators []config.GeneratorConfig) (*ProviderRegistry, error) {
	models := make([]config.ModelConfig, len(generators))
	for i, g := range generators {
		models[i] = config.ModelConfig{
			Provider:        g.Provider,
			Model:           g.Model,
			Endpoint:        g.Endpoint,
			APIKeyEnv:       g.APIKeyEnv,
			CostPer1MInput:  g.CostPer1MInput,
			CostPer1MOutput: g.CostPer1MOutput,
			TimeoutMs:       g.TimeoutMs,
			Capabilities:    g.Capabilities,
			DisableThinking: g.DisableThinking,
		}
	}
	return NewProviderRegistry(models)
}

// NewStreamersFromGenerators builds one streaming client per generator, keyed by
// its auction agent ID ("llm:<id>"). It differs from NewProviderRegistry, which
// keeps one client per provider TYPE for the non-streaming Acquire path: the
// streaming gateway (ADR-0018) must address each MODEL distinctly so the auction's
// StepAllocation winner and fallbacks each resolve to the right backend —
// including cross-provider fallback (a local Ollama primary failing over to a
// cloud OpenAI/Anthropic model). OllamaClient, OpenAIClient and AnthropicClient
// all implement domain.LLMStreamer, so any configured provider can serve the
// streaming agent path — previously only the Ollama generator was registered, so
// a config without a local model left agents with no streaming client at all.
func NewStreamersFromGenerators(generators []config.GeneratorConfig) (map[string]domain.LLMStreamer, error) {
	streamers := make(map[string]domain.LLMStreamer, len(generators))
	var firstErr error
	for _, g := range generators {
		var s domain.LLMStreamer
		switch g.Provider {
		case "ollama":
			s = &OllamaClient{BaseURL: g.Endpoint, Model: g.Model, TimeoutMs: g.TimeoutMs}
		case "openai":
			s = &OpenAIClient{Endpoint: g.Endpoint, Model: g.Model, APIKeyEnv: g.APIKeyEnv, TimeoutMs: g.TimeoutMs, DisableThinking: g.DisableThinking}
		case "anthropic":
			s = &AnthropicClient{Endpoint: g.Endpoint, Model: g.Model, APIKeyEnv: g.APIKeyEnv, TimeoutMs: g.TimeoutMs}
		case "gemini":
			s = &GeminiClient{Endpoint: g.Endpoint, Model: g.Model, APIKeyEnv: g.APIKeyEnv, TimeoutMs: g.TimeoutMs}
		default:
			if firstErr == nil {
				firstErr = fmt.Errorf("NewStreamersFromGenerators: unknown provider %q for generator %q", g.Provider, g.ID)
			}
			continue
		}
		streamers["llm:"+g.ID] = s
	}
	return streamers, firstErr
}

// NewProviderRegistry constructs a ProviderRegistry from config.ModelConfig
// entries. Each model is initialised and stored by provider name.
func NewProviderRegistry(models []config.ModelConfig) (*ProviderRegistry, error) {
	reg := &ProviderRegistry{}
	var err error

	for _, mc := range models {
		switch mc.Provider {
		case "ollama":
			reg.Ollama = &OllamaClient{
				BaseURL:   mc.Endpoint,
				Model:     mc.Model,
				TimeoutMs: mc.TimeoutMs,
			}
			if reg.OllamaEmbedder == nil {
				reg.OllamaEmbedder = &OllamaEmbedder{
					BaseURL:   mc.Endpoint,
					Model:     mc.Model,
					TimeoutMs: mc.TimeoutMs,
				}
			}
		case "openai":
			reg.OpenAI = &OpenAIClient{
				Endpoint:        mc.Endpoint,
				Model:           mc.Model,
				APIKeyEnv:       mc.APIKeyEnv,
				TimeoutMs:       mc.TimeoutMs,
				DisableThinking: mc.DisableThinking,
			}
		case "anthropic":
			reg.Anthropic = &AnthropicClient{
				Endpoint:  mc.Endpoint,
				Model:     mc.Model,
				APIKeyEnv: mc.APIKeyEnv,
				TimeoutMs: mc.TimeoutMs,
			}
		case "gemini":
			reg.Gemini = &GeminiClient{
				Endpoint:  mc.Endpoint,
				Model:     mc.Model,
				APIKeyEnv: mc.APIKeyEnv,
				TimeoutMs: mc.TimeoutMs,
			}
		default:
			err = fmt.Errorf("unknown LLM provider: %q", mc.Provider)
		}
	}

	return reg, err
}

// Generator returns the Generator for the given provider name.
func (r *ProviderRegistry) Generator(provider, model string) (domain.Generator, TokenUsageExtractor, error) {
	switch provider {
	case "ollama":
		if r.Ollama == nil {
			return nil, nil, fmt.Errorf("ollama client not initialized")
		}
		return r.Ollama, &ollamaExtractor{}, nil
	case "openai":
		if r.OpenAI == nil {
			return nil, nil, fmt.Errorf("openai client not initialized")
		}
		return r.OpenAI, &openaiExtractor{}, nil
	case "anthropic":
		if r.Anthropic == nil {
			return nil, nil, fmt.Errorf("anthropic client not initialized")
		}
		return r.Anthropic, &anthropicExtractor{}, nil
	case "gemini":
		if r.Gemini == nil {
			return nil, nil, fmt.Errorf("gemini client not initialized")
		}
		return r.Gemini, &geminiExtractor{}, nil
	default:
		return nil, nil, fmt.Errorf("unknown LLM provider: %q", provider)
	}
}

// PrimaryGenerator returns the Generator for the first available model,
// falling back to Ollama as the default local provider.
func (r *ProviderRegistry) PrimaryGenerator() (domain.Generator, error) {
	if r.Ollama != nil {
		return r.Ollama, nil
	}
	return nil, fmt.Errorf("no LLM provider available")
}

// Embedder returns the primary embedder (Ollama-only for now).
func (r *ProviderRegistry) Embedder() *OllamaEmbedder {
	return r.OllamaEmbedder
}
