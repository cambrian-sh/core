package llm

import (
	"fmt"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// NewClient creates a Generator + TokenUsageExtractor pair for the given model
// configuration. The provider field selects the client implementation.
func NewClient(cfg config.ModelConfig) (domain.Generator, TokenUsageExtractor, error) {
	switch cfg.Provider {
	case "ollama":
		c := &OllamaClient{
			BaseURL:   cfg.Endpoint,
			Model:     cfg.Model,
			TimeoutMs: cfg.TimeoutMs,
		}
		return c, &ollamaExtractor{}, nil
	case "openai":
		c := &OpenAIClient{
			Endpoint:        cfg.Endpoint,
			Model:           cfg.Model,
			APIKeyEnv:       cfg.APIKeyEnv,
			GeneratorID:     cfg.ID,
			TimeoutMs:       cfg.TimeoutMs,
			DisableThinking: cfg.DisableThinking,
			NativeTools:     cfg.NativeTools,
		}
		return c, &openaiExtractor{}, nil
	case "anthropic":
		c := &AnthropicClient{
			Endpoint:    cfg.Endpoint,
			Model:       cfg.Model,
			APIKeyEnv:   cfg.APIKeyEnv,
			GeneratorID: cfg.ID,
			TimeoutMs:   cfg.TimeoutMs,
		}
		return c, &anthropicExtractor{}, nil
	case "gemini":
		c := &GeminiClient{
			Endpoint:    cfg.Endpoint,
			Model:       cfg.Model,
			APIKeyEnv:   cfg.APIKeyEnv,
			GeneratorID: cfg.ID,
			TimeoutMs:   cfg.TimeoutMs,
		}
		return c, &geminiExtractor{}, nil
	default:
		return nil, nil, fmt.Errorf("unknown LLM provider: %q", cfg.Provider)
	}
}
