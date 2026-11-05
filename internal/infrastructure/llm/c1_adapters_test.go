package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// Wire-level tests for the C1 adapters (ADR-0139 D1): each asserts the exact
// request an SDK-built adapter puts on the wire and how the reply maps back.

type capturedRequest struct {
	mu     sync.Mutex
	path   string
	header http.Header
	body   map[string]any
	n      int
}

func (c *capturedRequest) get() (string, http.Header, map[string]any, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.path, c.header, c.body, c.n
}

// captureServer records the last request and answers with reply (JSON, or SSE
// when it starts with "event:" or "data:").
func captureServer(t *testing.T, reply string) (*httptest.Server, *capturedRequest) {
	t.Helper()
	cap := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		cap.mu.Lock()
		cap.path, cap.header, cap.body = r.URL.Path, r.Header.Clone(), body
		cap.n++
		cap.mu.Unlock()
		if strings.HasPrefix(reply, "event:") || strings.HasPrefix(reply, "data:") {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, cap
}

func f32(v float32) *float32 { return &v }

func drain(t *testing.T, ch <-chan domain.StreamChunk) (string, *domain.GenerateResponse) {
	t.Helper()
	var text strings.Builder
	var final *domain.GenerateResponse
	for c := range ch {
		text.WriteString(c.Text)
		if c.IsFinal {
			if strings.HasPrefix(c.Text, "stream_error") {
				t.Fatalf("stream failed: %s", c.Text)
			}
			final = c.Response
		}
	}
	return text.String(), final
}

const openAIReply = `{"id":"x","object":"chat.completion","created":1,"model":"served-model",
 "choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hello"}}],
 "usage":{"prompt_tokens":120,"completion_tokens":30,"total_tokens":150,"prompt_tokens_details":{"cached_tokens":100}}}`

// ── OpenAI-compatible ────────────────────────────────────────────────────────

func TestOpenAI_C1_WireRequest(t *testing.T) {
	t.Parallel()
	srv, cap := captureServer(t, openAIReply)
	c := &OpenAIClient{Endpoint: srv.URL + "/v1", Model: "m", TimeoutMs: 5000, NativeTools: true, DisableThinking: true}

	resp, err := c.GenerateRequest(context.Background(), domain.GenerateRequest{
		System:         []domain.SystemBlock{{Text: "you are terse", Cache: true}, {Text: "rules"}},
		Messages:       []domain.ModelMessage{domain.UserMessage("hi")},
		Tools:          []domain.ToolDefinition{{Name: "lookup", Description: "find", Parameters: []byte(`{"type":"object","properties":{"q":{"type":"string"}}}`)}},
		ToolChoice:     domain.ToolChoice{Mode: domain.ToolChoiceTool, Name: "lookup", DisableParallel: true},
		ResponseSchema: &domain.ResponseSchema{Name: "answer", Schema: []byte(`{"type":"object"}`)},
		Options:        domain.RequestOptions{MaxTokens: 256, Temperature: f32(0.2), Stop: []string{"END"}},
		ProviderExtensions: map[string]json.RawMessage{
			"openai:seed": json.RawMessage(`7`), "anthropic:metadata": json.RawMessage(`{}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	path, hdr, body, _ := cap.get()
	if path != "/v1/chat/completions" {
		t.Errorf("path = %q", path)
	}
	if hdr.Get("Authorization") != "" {
		t.Errorf("no credential configured: Authorization must be absent, got %q", hdr.Get("Authorization"))
	}
	msgs := body["messages"].([]any)
	sys := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "you are terse\n\nrules" {
		t.Errorf("system blocks must become one leading system message, got %v", sys)
	}
	if u := msgs[1].(map[string]any); u["role"] != "user" || u["content"] != "hi" {
		t.Errorf("user turn = %v", u)
	}
	tc := body["tool_choice"].(map[string]any)
	if tc["type"] != "function" || tc["function"].(map[string]any)["name"] != "lookup" {
		t.Errorf("named tool_choice = %v", tc)
	}
	if body["parallel_tool_calls"] != false {
		t.Errorf("parallel_tool_calls = %v, want false", body["parallel_tool_calls"])
	}
	rf := body["response_format"].(map[string]any)
	if rf["type"] != "json_schema" || rf["json_schema"].(map[string]any)["name"] != "answer" {
		t.Errorf("response_format = %v", rf)
	}
	if _, strict := rf["json_schema"].(map[string]any)["strict"]; strict {
		t.Error("strict must be sent only for a strict schema")
	}
	if body["max_tokens"] != float64(256) || body["temperature"].(float64) < 0.19 || body["temperature"].(float64) > 0.21 {
		t.Errorf("options: max_tokens=%v temperature=%v", body["max_tokens"], body["temperature"])
	}
	if stop := body["stop"].([]any); len(stop) != 1 || stop[0] != "END" {
		t.Errorf("stop = %v", body["stop"])
	}
	if th := body["thinking"].(map[string]any); th["type"] != "disabled" {
		t.Errorf("disable_thinking must still send thinking:disabled, got %v", body["thinking"])
	}
	if body["seed"] != float64(7) {
		t.Errorf("an openai: extension must reach the body, got %v", body["seed"])
	}
	if _, leaked := body["metadata"]; leaked {
		t.Error("an extension addressed to another provider must not be sent")
	}
	// Response mapping.
	if resp.Text != "hello" || resp.StopReason != domain.StopEndTurn || resp.Model != "served-model" {
		t.Errorf("response = %+v", resp)
	}
	if resp.Usage.Input != 120 || resp.Usage.Output != 30 || resp.Usage.CacheRead != 100 || resp.Usage.CacheWriteKnown {
		t.Errorf("usage = %+v (cached_tokens -> CacheRead, CacheWriteKnown=false)", resp.Usage)
	}
	// OpenAI-compatible caching is implicit: a cache mark is reported, not sent.
	if !contains(resp.Ignored, domain.FieldCacheBreakpoints) {
		t.Errorf("Ignored = %v, want cache_breakpoints", resp.Ignored)
	}
}

func TestOpenAI_C1_AmbientEnvKeyNeverSent(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-ambient-must-not-leak")
	srv, cap := captureServer(t, openAIReply)
	c := &OpenAIClient{Endpoint: srv.URL, Model: "m", TimeoutMs: 5000}
	if _, err := c.Generate(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if _, hdr, _, _ := cap.get(); hdr.Get("Authorization") != "" {
		t.Fatalf("the SDK's OPENAI_API_KEY autoload reached a non-OpenAI endpoint: %q", hdr.Get("Authorization"))
	}

	t.Setenv("CAMBRIAN_TEST_OPENAI_KEY", "sk-configured")
	c.APIKeyEnv = "CAMBRIAN_TEST_OPENAI_KEY"
	if _, err := c.Generate(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if _, hdr, _, _ := cap.get(); hdr.Get("Authorization") != "Bearer sk-configured" {
		t.Fatalf("configured credential not sent: %q", hdr.Get("Authorization"))
	}
}

// The haiku-local probe (2026-11-04): claudebox ignores temperature and stop and
// best-efforts max_tokens and response_format. Declared in config, the adapter
// reports — never silently drops — and refuses under strict.
func haikuLocalCaps() *domain.Capabilities {
	return &domain.Capabilities{
		SystemRole: domain.LevelEnforced, Tools: domain.LevelEnforced, ToolChoice: domain.LevelBestEffort,
		ParallelTools: domain.LevelBestEffort, ResponseSchema: domain.LevelBestEffort, MaxTokens: domain.LevelBestEffort,
		Sampling: domain.LevelUnsupported, Stop: domain.LevelUnsupported,
		CacheBreakpoints: domain.LevelUnsupported, Images: domain.LevelUnsupported,
	}
}

func TestOpenAI_C1_UnsupportedFieldsAreReportedNotSent(t *testing.T) {
	t.Parallel()
	srv, cap := captureServer(t, openAIReply)
	c := &OpenAIClient{Endpoint: srv.URL, Model: "claude-haiku-4-5", TimeoutMs: 5000, NativeTools: true, Caps: haikuLocalCaps()}

	resp, err := c.GenerateRequest(context.Background(), domain.GenerateRequest{
		Messages: []domain.ModelMessage{domain.UserMessage("hi")},
		Options:  domain.RequestOptions{MaxTokens: 100, Temperature: f32(0), Stop: []string{"X"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, body, _ := cap.get()
	for _, f := range []string{"temperature", "stop"} {
		if _, sent := body[f]; sent {
			t.Errorf("%s is unsupported on this generator and must not be sent", f)
		}
	}
	if body["max_tokens"] != float64(100) {
		t.Errorf("best-effort max_tokens must still be sent, got %v", body["max_tokens"])
	}
	if !contains(resp.Ignored, domain.FieldTemperature) || !contains(resp.Ignored, domain.FieldStop) {
		t.Errorf("Ignored = %v, want temperature and stop", resp.Ignored)
	}
	if contains(resp.Ignored, domain.FieldMaxTokens) {
		t.Error("a best-effort field is sent, not ignored")
	}
}

func TestOpenAI_C1_StrictRefusesWhatCannotBeEnforced(t *testing.T) {
	t.Parallel()
	srv, cap := captureServer(t, openAIReply)
	c := &OpenAIClient{Endpoint: srv.URL, Model: "m", TimeoutMs: 5000, Caps: haikuLocalCaps()}
	cases := map[string]domain.GenerateRequest{
		domain.FieldTemperature: {Options: domain.RequestOptions{Temperature: f32(0), Strict: true}},
		domain.FieldStop:        {Options: domain.RequestOptions{Stop: []string{"X"}, Strict: true}},
		// best-effort is not enough under strict: the caller asked for a guarantee.
		domain.FieldMaxTokens:      {Options: domain.RequestOptions{MaxTokens: 10, Strict: true}},
		domain.FieldResponseSchema: {ResponseSchema: &domain.ResponseSchema{Schema: []byte(`{}`), Strict: true}},
	}
	for field, req := range cases {
		req.Messages = []domain.ModelMessage{domain.UserMessage("hi")}
		_, err := c.GenerateRequest(context.Background(), req)
		if !errors.Is(err, domain.ErrUnsupported) || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: err = %v, want ErrUnsupported naming the field", field, err)
		}
	}
	if _, _, _, n := cap.get(); n != 0 {
		t.Errorf("a strict refusal must happen before any request is sent (%d sent)", n)
	}
}

func TestOpenAI_C1_ToolsOnANonToolGeneratorIsAnError(t *testing.T) {
	t.Parallel()
	c := &OpenAIClient{Endpoint: "http://127.0.0.1:1", Model: "m"} // native_tools off
	_, err := c.GenerateRequest(context.Background(), domain.GenerateRequest{
		Messages: []domain.ModelMessage{domain.UserMessage("hi")},
		Tools:    []domain.ToolDefinition{{Name: "t"}},
	})
	if !errors.Is(err, domain.ErrUnsupported) {
		t.Fatalf("tools are never silently dropped: err = %v", err)
	}
}

func TestOpenAI_C1_StreamAssemblesToolCallsAndUsage(t *testing.T) {
	t.Parallel()
	sse := strings.Join([]string{
		`data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m1","choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}`,
		`data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m1","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}`,
		`data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\""}}]},"finish_reason":null}]}`,
		`data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":\"go\"}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":4}}}`,
		`data: [DONE]`, ``,
	}, "\n\n")
	srv, cap := captureServer(t, sse)
	c := &OpenAIClient{Endpoint: srv.URL, Model: "m", NativeTools: true}
	ch, err := c.StreamRequest(context.Background(), domain.GenerateRequest{
		Messages: []domain.ModelMessage{domain.UserMessage("hi")},
		Tools:    []domain.ToolDefinition{{Name: "lookup", Parameters: []byte(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	text, final := drain(t, ch)
	if text != "Hello" {
		t.Errorf("streamed text = %q", text)
	}
	if final == nil {
		t.Fatal("the final chunk must carry the whole response")
	}
	if len(final.ToolCalls) != 1 || final.ToolCalls[0].ID != "call_1" || string(final.ToolCalls[0].Arguments) != `{"q":"go"}` {
		t.Errorf("tool calls = %+v", final.ToolCalls)
	}
	if final.StopReason != domain.StopToolUse || final.Usage.Input != 10 || final.Usage.CacheRead != 4 {
		t.Errorf("final = %+v", final)
	}
	if _, _, body, _ := cap.get(); body["stream_options"].(map[string]any)["include_usage"] != true {
		t.Error("streaming must request include_usage")
	}
}

func TestOpenAI_C1_HTTPErrorIsAProviderError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"bad tool schema"}}`)
	}))
	defer srv.Close()
	c := &OpenAIClient{Endpoint: srv.URL, Model: "m"}
	_, err := c.Generate(context.Background(), "hi")
	var pe *ProviderError
	if !errors.As(err, &pe) || pe.Status != http.StatusBadRequest || !strings.Contains(err.Error(), "bad tool schema") {
		t.Fatalf("err = %v, want a ProviderError carrying status and body", err)
	}
	if _, serr := c.GenerateStream(context.Background(), "hi"); serr == nil {
		t.Fatal("a refused stream must fail synchronously, as the hand-written streamer did")
	}
}

// ── Anthropic ────────────────────────────────────────────────────────────────

const anthropicReply = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-served",
 "content":[{"type":"text","text":"done "},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":"go"}}],
 "stop_reason":"tool_use","stop_sequence":null,
 "usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":200,"cache_creation_input_tokens":50}}`

func cacheMarks(t *testing.T, body map[string]any) (system []int, messages []int, ttls []string) {
	t.Helper()
	for i, b := range body["system"].([]any) {
		if cc, ok := b.(map[string]any)["cache_control"].(map[string]any); ok {
			system = append(system, i)
			if s, _ := cc["ttl"].(string); s != "" {
				ttls = append(ttls, s)
			}
		}
	}
	for i, m := range body["messages"].([]any) {
		for _, blk := range m.(map[string]any)["content"].([]any) {
			if cc, ok := blk.(map[string]any)["cache_control"].(map[string]any); ok {
				messages = append(messages, i)
				if s, _ := cc["ttl"].(string); s != "" {
					ttls = append(ttls, s)
				}
			}
		}
	}
	return
}

func TestAnthropic_C1_WireRequestAndCacheLayout(t *testing.T) {
	srv, cap := captureServer(t, anthropicReply)
	t.Setenv("CAMBRIAN_TEST_ANTHROPIC_KEY_WIRE", "ak-test")
	c := &AnthropicClient{Endpoint: srv.URL + "/v1", Model: "claude-x", APIKeyEnv: "CAMBRIAN_TEST_ANTHROPIC_KEY_WIRE", TimeoutMs: 5000, MaxTokens: 777, NativeTools: true}

	resp, err := c.GenerateRequest(context.Background(), domain.GenerateRequest{
		System: []domain.SystemBlock{{Text: "stable role", Cache: true}, {Text: "stable rules", Cache: true}, {Text: "volatile"}},
		Messages: []domain.ModelMessage{
			domain.UserMessage("u1"),
			{Role: domain.RoleAssistant, Content: "a1"},
			domain.UserMessage("u2"),
			{Role: domain.RoleAssistant, ToolCalls: []domain.ModelToolCall{
				{ID: "toolu_a", Name: "lookup", Arguments: []byte(`{"q":"x"}`)},
				{ID: "toolu_b", Name: "lookup", Arguments: []byte(`{"q":"y"}`)}}},
			{Role: domain.RoleTool, ToolCallID: "toolu_a", Content: "ra"},
			{Role: domain.RoleTool, ToolCallID: "toolu_b", Content: "rb"},
		},
		Tools:          []domain.ToolDefinition{{Name: "lookup", Description: "find", Parameters: []byte(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"],"additionalProperties":false}`)}},
		ResponseSchema: &domain.ResponseSchema{Schema: []byte(`{"type":"object"}`)},
		Options:        domain.RequestOptions{Temperature: f32(0.5), Stop: []string{"STOP"}},
		CacheTTL:       domain.CacheTTLLong,
	})
	if err != nil {
		t.Fatal(err)
	}
	path, hdr, body, _ := cap.get()
	if path != "/v1/messages" || hdr.Get("X-Api-Key") != "ak-test" {
		t.Errorf("path=%q x-api-key=%q", path, hdr.Get("X-Api-Key"))
	}
	if body["max_tokens"] != float64(777) {
		t.Errorf("max_tokens = %v, want the generator's configured default", body["max_tokens"])
	}
	sysIdx, msgIdx, ttls := cacheMarks(t, body)
	// Hermes layout: ONE system breakpoint, on the last cache-marked block (index 1).
	if len(sysIdx) != 1 || sysIdx[0] != 1 {
		t.Errorf("system breakpoints at %v, want [1]", sysIdx)
	}
	msgs := body["messages"].([]any)
	// Tool turns merge into one user message: u1, a1, u2, assistant(tool_use x2), user(tool_result x2).
	if len(msgs) != 5 {
		t.Fatalf("want 5 wire messages (tool results merged), got %d", len(msgs))
	}
	last := msgs[4].(map[string]any)["content"].([]any)
	if len(last) != 2 || last[0].(map[string]any)["type"] != "tool_result" || last[1].(map[string]any)["tool_use_id"] != "toolu_b" {
		t.Errorf("tool results = %v", last)
	}
	// Rolling breakpoints on the last 3 messages: 4 marks in total, never more.
	if len(msgIdx) != 3 || msgIdx[0] != 2 || msgIdx[2] != 4 {
		t.Errorf("message breakpoints at %v, want the last three [2 3 4]", msgIdx)
	}
	if len(sysIdx)+len(msgIdx) > maxCacheBreakpoints {
		t.Errorf("%d breakpoints exceed Anthropic's limit of 4", len(sysIdx)+len(msgIdx))
	}
	for _, ttl := range ttls {
		if ttl != "1h" {
			t.Errorf("CacheTTLLong must send ttl 1h, got %q", ttl)
		}
	}
	tool := body["tools"].([]any)[0].(map[string]any)
	schema := tool["input_schema"].(map[string]any)
	if schema["type"] != "object" || schema["additionalProperties"] != false || len(schema["required"].([]any)) != 1 {
		t.Errorf("input_schema must carry the raw schema unchanged, got %v", schema)
	}
	if f := body["output_config"].(map[string]any)["format"].(map[string]any); f["type"] != "json_schema" {
		t.Errorf("structured output = %v", f)
	}
	if body["temperature"] != 0.5 || body["stop_sequences"].([]any)[0] != "STOP" {
		t.Errorf("temperature=%v stop=%v", body["temperature"], body["stop_sequences"])
	}
	// Response mapping.
	if resp.Text != "done " || len(resp.ToolCalls) != 1 || string(resp.ToolCalls[0].Arguments) != `{"q":"go"}` {
		t.Errorf("response = %+v", resp)
	}
	if resp.StopReason != domain.StopToolUse || resp.Model != "claude-served" {
		t.Errorf("stop=%q model=%q", resp.StopReason, resp.Model)
	}
	u := resp.Usage
	if u.Input != 260 || u.CacheRead != 200 || u.CacheWrite != 50 || !u.CacheWriteKnown || u.Output != 5 {
		t.Errorf("usage = %+v (input = input+cache_read+cache_write)", u)
	}
	if len(resp.Ignored) != 0 {
		t.Errorf("Anthropic honours everything sent here; Ignored = %v", resp.Ignored)
	}
}

// The legacy shim must send what the hand-written client sent: one user turn,
// max_tokens 4096 (now from config.AnthropicDefaultMaxTokens), no system, no cache.
func TestAnthropic_C1_LegacyShimUnchanged(t *testing.T) {
	t.Parallel()
	srv, cap := captureServer(t, `{"id":"m","type":"message","role":"assistant","model":"c","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	c := &AnthropicClient{Endpoint: srv.URL + "/v1", Model: "claude-x", TimeoutMs: 5000}
	out, err := c.Generate(context.Background(), "prompt")
	if err != nil || out != "ok" {
		t.Fatalf("Generate = %q, %v", out, err)
	}
	_, _, body, _ := cap.get()
	if body["max_tokens"] != float64(4096) {
		t.Errorf("max_tokens = %v, want 4096", body["max_tokens"])
	}
	if _, has := body["system"]; has {
		t.Error("the shim sends no system field")
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("want one user turn, got %v", msgs)
	}
	if strings.Contains(mustJSON(t, body), "cache_control") {
		t.Error("caching is opt-in: no breakpoint without a cache-marked system block")
	}
}

func TestAnthropic_C1_Stream(t *testing.T) {
	t.Parallel()
	sse := strings.Join([]string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-s\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":10,\"output_tokens\":1,\"cache_read_input_tokens\":100,\"cache_creation_input_tokens\":20}}}",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi \"}}",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"there\"}}",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":7}}",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}", "",
	}, "\n\n")
	srv, _ := captureServer(t, sse)
	c := &AnthropicClient{Endpoint: srv.URL, Model: "claude-x"}
	ch, err := c.GenerateStream(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	var finalChunk domain.StreamChunk
	var text strings.Builder
	for chunk := range ch {
		text.WriteString(chunk.Text)
		if chunk.IsFinal {
			finalChunk = chunk
		}
	}
	if text.String() != "Hi there" || finalChunk.Response == nil {
		t.Fatalf("text=%q final=%+v", text.String(), finalChunk)
	}
	if finalChunk.Response.StopReason != domain.StopEndTurn || finalChunk.Response.Usage.CacheRead != 100 {
		t.Errorf("final response = %+v", finalChunk.Response)
	}
	// Legacy consumers read the Usage* fields: input = 10+100+20, output = 7.
	if finalChunk.UsagePromptTokens != 130 || finalChunk.UsageCompletionTokens != 7 || finalChunk.UsageCacheReadTokens != 100 {
		t.Errorf("legacy usage fields = %d/%d/%d", finalChunk.UsagePromptTokens, finalChunk.UsageCompletionTokens, finalChunk.UsageCacheReadTokens)
	}
}

// ── Gemini ───────────────────────────────────────────────────────────────────

func TestGemini_C1_WireRequestAndMapping(t *testing.T) {
	t.Setenv("CAMBRIAN_TEST_GEMINI_KEY", "gk-test")
	srv, cap := captureServer(t, `{"candidates":[{"content":{"role":"model","parts":[
		{"text":"calling"},{"functionCall":{"id":"fc1","name":"lookup","args":{"q":"go"}},"thoughtSignature":"c2ln"}]},
		"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":40,"candidatesTokenCount":6,"cachedContentTokenCount":30,"totalTokenCount":46},
		"modelVersion":"gemini-served"}`)
	c := &GeminiClient{Endpoint: srv.URL + "/v1beta", Model: "models/gemini-x", APIKeyEnv: "CAMBRIAN_TEST_GEMINI_KEY", TimeoutMs: 5000, NativeTools: true}

	resp, err := c.GenerateRequest(context.Background(), domain.GenerateRequest{
		System:         []domain.SystemBlock{{Text: "be brief", Cache: true}},
		Messages:       []domain.ModelMessage{domain.UserMessage("hi")},
		Tools:          []domain.ToolDefinition{{Name: "lookup", Description: "find", Parameters: []byte(`{"type":"object","properties":{"q":{"type":"string"}}}`)}},
		ResponseSchema: &domain.ResponseSchema{Schema: []byte(`{"type":"object"}`)},
		Options:        domain.RequestOptions{MaxTokens: 64, Temperature: f32(0.1), Stop: []string{"END"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	path, hdr, body, _ := cap.get()
	if path != "/v1beta/models/gemini-x:generateContent" || hdr.Get("X-Goog-Api-Key") != "gk-test" {
		t.Errorf("path=%q key=%q", path, hdr.Get("X-Goog-Api-Key"))
	}
	si := body["systemInstruction"].(map[string]any)["parts"].([]any)[0].(map[string]any)
	if si["text"] != "be brief" {
		t.Errorf("systemInstruction = %v", si)
	}
	fd := body["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)
	if fd["name"] != "lookup" || fd["parametersJsonSchema"] == nil {
		t.Errorf("functionDeclarations = %v", fd)
	}
	gc := body["generationConfig"].(map[string]any)
	if gc["responseMimeType"] != "application/json" || gc["responseJsonSchema"] == nil {
		t.Errorf("structured output config = %v", gc)
	}
	if gc["maxOutputTokens"] != float64(64) || gc["stopSequences"].([]any)[0] != "END" || gc["temperature"] == nil {
		t.Errorf("generationConfig = %v", gc)
	}
	if resp.Text != "calling" || len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "lookup" {
		t.Fatalf("response = %+v", resp)
	}
	if string(resp.ToolCalls[0].Signature) != "sig" {
		t.Errorf("thoughtSignature must be kept for the echo, got %q", resp.ToolCalls[0].Signature)
	}
	// Gemini says STOP on a function-call turn; the normalised reason must not.
	if resp.StopReason != domain.StopToolUse {
		t.Errorf("StopReason = %q, want tool_use", resp.StopReason)
	}
	if resp.Usage.Input != 40 || resp.Usage.CacheRead != 30 || resp.Model != "gemini-served" {
		t.Errorf("usage=%+v model=%q", resp.Usage, resp.Model)
	}
	if !contains(resp.Ignored, domain.FieldCacheBreakpoints) {
		t.Errorf("Ignored = %v, want cache_breakpoints", resp.Ignored)
	}
}

func TestGemini_C1_ToolResultEchoesNameAndSignature(t *testing.T) {
	t.Parallel()
	contents, err := toGeminiContents([]domain.ModelMessage{
		domain.UserMessage("q"),
		{Role: domain.RoleAssistant, ToolCalls: []domain.ModelToolCall{{ID: "fc1", Name: "lookup", Arguments: []byte(`{"q":"go"}`), Signature: []byte("sig")}}},
		{Role: domain.RoleTool, ToolCallID: "fc1", Content: "plain text result"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(contents) != 3 {
		t.Fatalf("want 3 contents, got %d", len(contents))
	}
	call := contents[1].Parts[0]
	if call.FunctionCall == nil || string(call.ThoughtSignature) != "sig" {
		t.Errorf("assistant call part = %+v", call)
	}
	fr := contents[2].Parts[0].FunctionResponse
	if fr == nil || fr.Name != "lookup" || fr.Response["output"] != "plain text result" {
		t.Errorf("function response = %+v", fr)
	}
}

// ── Ollama (via /v1) ─────────────────────────────────────────────────────────

func TestOllama_C1_LegacyGenerateKeepsJSONModeAndTemperatureZero(t *testing.T) {
	t.Parallel()
	srv, cap := captureServer(t, `{"id":"x","object":"chat.completion","created":1,"model":"qwen3:8b",
		"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"{\"ok\":true}"}}],
		"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	c := &OllamaClient{BaseURL: srv.URL, Model: "qwen3:8b", TimeoutMs: 5000}
	out, err := c.Generate(context.Background(), "give json")
	if err != nil || out != `{"ok":true}` {
		t.Fatalf("Generate = %q, %v", out, err)
	}
	path, hdr, body, _ := cap.get()
	if path != "/v1/chat/completions" {
		t.Errorf("path = %q, want Ollama's OpenAI-compatible /v1", path)
	}
	if hdr.Get("Authorization") != "" {
		t.Error("a local Ollama gets no credential")
	}
	if rf := body["response_format"].(map[string]any); rf["type"] != "json_object" {
		t.Errorf("JSON mode (the old format:json) = %v", rf)
	}
	if body["temperature"] != float64(0) {
		t.Errorf("temperature = %v, want 0 as before", body["temperature"])
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
