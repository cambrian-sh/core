package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/cambrian-sh/core/domain"
)

type OpenAIClient struct {
	Endpoint  string
	Model     string
	APIKeyEnv string
	TimeoutMs int
	// DisableThinking sends thinking:{"type":"disabled"} to suppress server-side
	// reasoning on OpenAI-compat reasoning models (deepseek-v4-flash on opencode).
	DisableThinking bool
	// NativeTools declares that this endpoint honours the tool-calling API
	// (ADR-0097 D2). Default false: sending `tools` to an endpoint that ignores them
	// yields a model that never calls anything, which reads as a model-quality
	// problem rather than a configuration one.
	NativeTools bool
}

// NativeToolsEnabled implements domain.ToolCallingReporter.
func (c *OpenAIClient) NativeToolsEnabled() bool { return c.NativeTools }

// openAIThinking is the opencode/deepseek reasoning toggle. Type "disabled"
// suppresses reasoning-token generation (faster; no empty-content-from-budget).
type openAIThinking struct {
	Type string `json:"type"`
}

func disabledThinking(disable bool) *openAIThinking {
	if !disable {
		return nil
	}
	return &openAIThinking{Type: "disabled"}
}

type openAIChatRequest struct {
	Model    string          `json:"model"`
	Messages []openAIChatMsg `json:"messages"`
	Thinking *openAIThinking `json:"thinking,omitempty"`
}

type openAIChatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// Tool-calling conversation fields (ADR-0097 D8). Both omitempty: a plain text
	// completion must serialize byte-identically to before this change.
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIChatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func (c *OpenAIClient) Generate(ctx context.Context, prompt string) (string, error) {
	timeout := time.Duration(c.TimeoutMs) * time.Millisecond
	httpClient := &http.Client{Timeout: timeout, Transport: sharedLLMTransport}

	reqBody := openAIChatRequest{
		Model: c.Model,
		Messages: []openAIChatMsg{
			{Role: "user", Content: prompt},
		},
		Thinking: disabledThinking(c.DisableThinking),
	}
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/chat/completions", c.Endpoint), bytes.NewBuffer(jsonData))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey := os.Getenv(c.APIKeyEnv); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("openai: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var chatResp openAIChatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return "", err
	}
	if len(chatResp.Choices) == 0 {
		return "", fmt.Errorf("openai: no choices in response")
	}
	return chatResp.Choices[0].Message.Content, nil
}

// ── Native tool-calling (ADR-0097 Phase A) ───────────────────────────────────

// openAIToolDef / openAIFunctionDef mirror the OpenAI `tools` request shape.
type openAIFunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type openAIToolDef struct {
	Type     string            `json:"type"` // always "function"
	Function openAIFunctionDef `json:"function"`
}

// openAIToolCall mirrors the response shape. Arguments is a JSON *string*, not an
// object — the provider double-encodes it, and both configured models were verified
// to do so on 2026-07-28.
type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAIToolChatRequest struct {
	Model      string          `json:"model"`
	Messages   []openAIChatMsg `json:"messages"`
	Thinking   *openAIThinking `json:"thinking,omitempty"`
	Tools      []openAIToolDef `json:"tools,omitempty"`
	ToolChoice string          `json:"tool_choice,omitempty"`
}

type openAIToolChatResponse struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content   string           `json:"content"`
			ToolCalls []openAIToolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
}

// normalizeOpenAIFinishReason maps OpenAI's vocabulary onto domain.StopReason.
//
// Anything unrecognised becomes StopUnknown, which is NOT finished — a provider we
// have not seen before must degrade to "keep going", never to "done".
func normalizeOpenAIFinishReason(r string) domain.StopReason {
	switch r {
	case "tool_calls", "function_call":
		return domain.StopToolUse
	case "stop":
		return domain.StopEndTurn
	case "length":
		return domain.StopMaxTokens
	case "content_filter":
		return domain.StopRefusal
	default:
		return domain.StopUnknown
	}
}

// GenerateWithTools implements domain.ToolCallingGenerator.
//
// Verified live against the configured endpoint (2026-07-28): both deepseek-v4-flash
// and mimo-v2.5 return finish_reason "tool_calls" with a populated tool_calls array.
func (c *OpenAIClient) GenerateWithTools(
	ctx context.Context, messages []domain.ModelMessage, tools []domain.ToolDefinition,
) (domain.ModelTurn, error) {
	var zero domain.ModelTurn
	// Refuse rather than quietly falling back to a plain completion: a caller that
	// reached here without checking SupportsToolCalling has a bug, and silently
	// dropping the tools would hand it a model that never calls anything.
	if !c.NativeTools {
		return zero, fmt.Errorf("openai: native tool-calling not enabled for model %q (set native_tools)", c.Model)
	}

	reqBody := openAIToolChatRequest{
		Model:    c.Model,
		Messages: toOpenAIMessages(messages),
		Thinking: disabledThinking(c.DisableThinking),
	}
	for _, t := range tools {
		reqBody.Tools = append(reqBody.Tools, openAIToolDef{
			Type: "function",
			Function: openAIFunctionDef{
				Name: t.Name, Description: t.Description, Parameters: json.RawMessage(t.Parameters),
			},
		})
	}
	// Only constrain the choice when tools are actually offered; sending
	// tool_choice with an empty tools array is rejected by some gateways.
	if len(reqBody.Tools) > 0 {
		reqBody.ToolChoice = "auto"
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return zero, err
	}

	httpClient := &http.Client{
		Timeout:   time.Duration(c.TimeoutMs) * time.Millisecond,
		Transport: sharedLLMTransport,
	}
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/chat/completions", c.Endpoint), bytes.NewBuffer(jsonData))
	if err != nil {
		return zero, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey := os.Getenv(c.APIKeyEnv); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return zero, err
	}
	if resp.StatusCode != http.StatusOK {
		// A gateway rejection on a tools request says only "Upstream request failed"
		// and names no field, so log what we OFFERED. Tool names and their schemas
		// are the field that gets rejected in practice — twice already: a name
		// containing ':' or '/', and a schema missing its top-level "type": "object".
		//
		// The prompt is deliberately NOT logged. It carries user content, and it has
		// never been the cause; the tool envelope is both the likely culprit and the
		// part that is safe to record.
		names := make([]string, 0, len(reqBody.Tools))
		for _, t := range reqBody.Tools {
			names = append(names, t.Function.Name)
		}
		slog.Error("openai: request rejected",
			"status", resp.StatusCode, "model", c.Model,
			"tool_count", len(reqBody.Tools), "tool_names", names,
			"response", string(body))
		return zero, fmt.Errorf("openai: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var chatResp openAIToolChatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return zero, err
	}
	if len(chatResp.Choices) == 0 {
		return zero, fmt.Errorf("openai: no choices in response")
	}

	ch := chatResp.Choices[0]
	out := domain.ModelTurn{
		Text:       ch.Message.Content,
		StopReason: normalizeOpenAIFinishReason(ch.FinishReason),
	}
	for _, tc := range ch.Message.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, domain.ModelToolCall{
			ID:   tc.ID,
			Name: tc.Function.Name,
			// Arguments arrive as a JSON string; forward the bytes unparsed.
			// Schema validation belongs to whoever owns the tool.
			Arguments: []byte(tc.Function.Arguments),
		})
	}
	return out, nil
}

// toOpenAIMessages renders the conversation onto the wire shape.
//
// An assistant turn carries its tool_calls; a tool turn carries the id of the call it
// answers. Dropping either — which the first cut of Phase B did by sending one user
// message per round — leaves the model unable to see its own call or its result.
func toOpenAIMessages(messages []domain.ModelMessage) []openAIChatMsg {
	out := make([]openAIChatMsg, 0, len(messages))
	for _, m := range messages {
		msg := openAIChatMsg{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			call := openAIToolCall{ID: tc.ID, Type: "function"}
			call.Function.Name = tc.Name
			// Arguments go back as the JSON STRING the provider sent, not re-encoded:
			// round-tripping through a map would reorder keys and change the bytes the
			// provider matches against its own record of the call.
			call.Function.Arguments = string(tc.Arguments)
			msg.ToolCalls = append(msg.ToolCalls, call)
		}
		out = append(out, msg)
	}
	return out
}
