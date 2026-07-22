package llm_test

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/llm"
)

func TestNewClient_Ollama(t *testing.T) {
	cfg := config.ModelConfig{
		Provider:  "ollama",
		Model:     "llama3",
		Endpoint:  "http://localhost:11434",
		TimeoutMs: 60000,
	}
	client, extractor, err := llm.NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient(ollama): %v", err)
	}
	if client == nil {
		t.Fatal("want non-nil client")
	}
	if extractor == nil {
		t.Fatal("want non-nil TokenUsageExtractor")
	}
	_, genErr := client.Generate(context.Background(), "test")
	if genErr == nil {
		t.Log("Generate returned nil error (no server running, expected)")
	}
}

func TestNewClient_OpenAI(t *testing.T) {
	cfg := config.ModelConfig{
		Provider:        "openai",
		Model:           "gpt-4o",
		Endpoint:        "https://api.openai.com/v1",
		APIKeyEnv:       "OPENAI_API_KEY",
		CostPer1MInput:  5.0,
		CostPer1MOutput: 15.0,
		TimeoutMs:       30000,
	}
	client, extractor, err := llm.NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient(openai): %v", err)
	}
	if client == nil {
		t.Fatal("want non-nil client")
	}
	if extractor == nil {
		t.Fatal("want non-nil TokenUsageExtractor")
	}
}

func TestNewClient_Anthropic(t *testing.T) {
	cfg := config.ModelConfig{
		Provider:        "anthropic",
		Model:           "claude-sonnet-4-20250514",
		Endpoint:        "https://api.anthropic.com/v1",
		APIKeyEnv:       "ANTHROPIC_API_KEY",
		CostPer1MInput:  3.0,
		CostPer1MOutput: 15.0,
		TimeoutMs:       30000,
	}
	client, extractor, err := llm.NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient(anthropic): %v", err)
	}
	if client == nil {
		t.Fatal("want non-nil client")
	}
	if extractor == nil {
		t.Fatal("want non-nil TokenUsageExtractor")
	}
}

func TestNewClient_UnknownProvider(t *testing.T) {
	// NOTE: "gemini" used to stand in for "unknown" here, but it is a supported
	// provider in the factory — use a provider that genuinely has no case.
	cfg := config.ModelConfig{
		Provider:  "no-such-provider",
		Model:     "whatever",
		Endpoint:  "https://example.invalid",
		TimeoutMs: 30000,
	}
	_, _, err := llm.NewClient(cfg)
	if err == nil {
		t.Fatal("want error for unknown provider, got nil")
	}
}

func TestTokenUsageExtractor_Interface(t *testing.T) {
	_, extractor, err := llm.NewClient(config.ModelConfig{
		Provider:  "ollama",
		Model:     "llama3",
		Endpoint:  "http://localhost:11434",
		TimeoutMs: 60000,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	usage, err := extractor.Extract([]byte(`{"response":"ok"}`))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if usage.PromptTokens < 0 || usage.CompletionTokens < 0 || usage.TotalTokens < 0 {
		t.Errorf("token counts must be non-negative: %+v", usage)
	}
}

func TestOllamaExtractor(t *testing.T) {
	_, extractor, err := llm.NewClient(config.ModelConfig{Provider: "ollama", Model: "llama3", Endpoint: "http://localhost:11434", TimeoutMs: 60000})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	usage, err := extractor.Extract([]byte(`{"prompt_eval_count":150,"eval_count":80}`))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if usage.PromptTokens != 150 || usage.CompletionTokens != 80 || usage.TotalTokens != 230 {
		t.Errorf("want 150/80/230, got %d/%d/%d", usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens)
	}
}

func TestOpenAIExtractor(t *testing.T) {
	_, extractor, err := llm.NewClient(config.ModelConfig{Provider: "openai", Model: "gpt-4o", Endpoint: "https://api.openai.com/v1", TimeoutMs: 30000})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	usage, err := extractor.Extract([]byte(`{"usage":{"prompt_tokens":200,"completion_tokens":100,"total_tokens":300}}`))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if usage.PromptTokens != 200 || usage.CompletionTokens != 100 || usage.TotalTokens != 300 {
		t.Errorf("want 200/100/300, got %d/%d/%d", usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens)
	}
}

func TestAnthropicExtractor(t *testing.T) {
	_, extractor, err := llm.NewClient(config.ModelConfig{Provider: "anthropic", Model: "claude", Endpoint: "https://api.anthropic.com", TimeoutMs: 30000})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	usage, err := extractor.Extract([]byte(`{"usage":{"input_tokens":120,"output_tokens":60}}`))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if usage.PromptTokens != 120 || usage.CompletionTokens != 60 || usage.TotalTokens != 180 {
		t.Errorf("want 120/60/180, got %d/%d/%d", usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens)
	}
}

func TestCalculateCost(t *testing.T) {
	usage := llm.TokenUsage{PromptTokens: 500000, CompletionTokens: 500000}
	cost := llm.CalculateCost(usage, 5.0, 15.0)
	if cost < 9.9 || cost > 10.1 {
		t.Errorf("want ~10.0 (0.5*5 + 0.5*15), got %v", cost)
	}
}

func TestCalculateCost_ZeroTokens(t *testing.T) {
	cost := llm.CalculateCost(llm.TokenUsage{}, 5.0, 15.0)
	if cost != 0.0 {
		t.Errorf("want 0.0, got %v", cost)
	}
}
