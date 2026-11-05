package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// OllamaClient serves a local Ollama generator through Ollama's OpenAI-compatible
// /v1 endpoint and the openai-go adapter (ADR-0139 D1, decision: no separate Ollama
// SDK dependency). It replaces the hand-written /api/generate client.
//
// The embedder is NOT here: OllamaEmbedder keeps its own /api/embed client.
type OllamaClient struct {
	BaseURL   string // e.g. http://localhost:11434 (no /v1)
	Model     string
	TimeoutMs int
	// NativeTools gates tool calling (ADR-0097 D2); Ollama supports tools only on
	// models trained for them.
	NativeTools bool
	// MaxTokens is the output cap sent when a request sets none; 0 sends none.
	MaxTokens int
	// Caps overrides the declared capabilities when non-nil.
	Caps *domain.Capabilities
}

// ollamaJSONMode is what the legacy Generate shim asks for: JSON mode without a
// schema, the /v1 equivalent of the /api/generate `"format": "json"` the
// hand-written client always sent.
var ollamaJSONMode = json.RawMessage(`{"type":"json_object"}`)

func (c *OllamaClient) inner() *OpenAIClient {
	caps := c.Capabilities()
	return &OpenAIClient{
		Endpoint:    strings.TrimRight(c.BaseURL, "/") + "/v1",
		Model:       c.Model,
		TimeoutMs:   c.TimeoutMs,
		NativeTools: c.NativeTools,
		MaxTokens:   c.MaxTokens,
		Caps:        &caps,
		provider:    "ollama",
	}
}

// Capabilities implements domain.RequestGenerator. Ollama grammar-constrains
// json_schema output, but older servers and some models ignore it, so it stays
// best-effort and the provider layer validates it.
func (c *OllamaClient) Capabilities() domain.Capabilities {
	if c.Caps != nil {
		return *c.Caps
	}
	tools := domain.LevelUnsupported
	if c.NativeTools {
		tools = domain.LevelEnforced
	}
	return domain.Capabilities{
		SystemRole:       domain.LevelEnforced,
		Tools:            tools,
		ToolChoice:       domain.LevelBestEffort,
		ParallelTools:    domain.LevelBestEffort,
		ResponseSchema:   domain.LevelBestEffort,
		MaxTokens:        domain.LevelEnforced,
		Sampling:         domain.LevelEnforced,
		Stop:             domain.LevelEnforced,
		CacheBreakpoints: domain.LevelUnsupported,
		Images:           domain.LevelUnsupported,
	}
}

// NativeToolsEnabled implements domain.ToolCallingReporter.
func (c *OllamaClient) NativeToolsEnabled() bool { return c.Capabilities().Tools.Sent() }

// RequestsEnabled implements domain.RequestGeneratorReporter.
func (c *OllamaClient) RequestsEnabled() bool { return true }

// GenerateRequest implements domain.RequestGenerator.
func (c *OllamaClient) GenerateRequest(ctx context.Context, req domain.GenerateRequest) (domain.GenerateResponse, error) {
	return c.inner().GenerateRequest(ctx, req)
}

// StreamRequest implements domain.RequestGenerator.
func (c *OllamaClient) StreamRequest(ctx context.Context, req domain.GenerateRequest) (<-chan domain.StreamChunk, error) {
	return c.inner().StreamRequest(ctx, req)
}

// legacyRequest is what the pre-C1 Ollama client sent for a prompt: temperature 0
// (both paths) and, for the non-streaming one, JSON mode.
func legacyOllamaRequest(prompt string, jsonMode bool) domain.GenerateRequest {
	zero := float32(0)
	req := domain.UserPrompt(prompt)
	req.Options.Temperature = &zero
	if jsonMode {
		req.ProviderExtensions = map[string]json.RawMessage{"ollama:response_format": ollamaJSONMode}
	}
	return req
}

// Generate implements domain.Generator with the legacy client's behaviour: JSON
// mode, temperature 0, and an empty reply is an error (ADR-0042 D4 — a silent empty
// success broke callers three layers up).
func (c *OllamaClient) Generate(ctx context.Context, prompt string) (string, error) {
	resp, err := c.GenerateRequest(ctx, legacyOllamaRequest(prompt, true))
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(resp.Text) == "" {
		return "", fmt.Errorf("ollama: empty response from model %q", c.Model)
	}
	return resp.Text, nil
}

// GenerateWithTools implements domain.ToolCallingGenerator.
func (c *OllamaClient) GenerateWithTools(
	ctx context.Context, messages []domain.ModelMessage, tools []domain.ToolDefinition,
) (domain.ModelTurn, error) {
	if !c.NativeToolsEnabled() {
		return domain.ModelTurn{}, fmt.Errorf("ollama: native tool-calling not enabled for model %q (set native_tools)", c.Model)
	}
	return shimGenerateWithTools(ctx, c, messages, tools)
}

// GenerateStream implements domain.LLMStreamer (temperature 0, as before).
func (c *OllamaClient) GenerateStream(ctx context.Context, prompt string) (<-chan domain.StreamChunk, error) {
	return c.StreamRequest(ctx, legacyOllamaRequest(prompt, false))
}

var (
	_ domain.RequestGenerator     = (*OllamaClient)(nil)
	_ domain.ToolCallingGenerator = (*OllamaClient)(nil)
	_ domain.LLMStreamer          = (*OllamaClient)(nil)
)
