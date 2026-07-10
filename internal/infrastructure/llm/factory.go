package llm

import (
	"fmt"

	"github.com/cambrian-sh/cambrian-runtime/internal/config"
	"github.com/cambrian-sh/cambrian-runtime/domain"
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
			TimeoutMs:       cfg.TimeoutMs,
			DisableThinking: cfg.DisableThinking,
		}
		return c, &openaiExtractor{}, nil
	case "anthropic":
		c := &AnthropicClient{
			Endpoint:  cfg.Endpoint,
			Model:     cfg.Model,
			APIKeyEnv: cfg.APIKeyEnv,
			TimeoutMs: cfg.TimeoutMs,
		}
		return c, &anthropicExtractor{}, nil
	case "gemini":
		c := &GeminiClient{
			Endpoint:  cfg.Endpoint,
			Model:     cfg.Model,
			APIKeyEnv: cfg.APIKeyEnv,
			TimeoutMs: cfg.TimeoutMs,
		}
		return c, &geminiExtractor{}, nil
	default:
		return nil, nil, fmt.Errorf("unknown LLM provider: %q", cfg.Provider)
	}
}
