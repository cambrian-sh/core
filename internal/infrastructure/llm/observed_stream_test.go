package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// ADR-0139 amendment 2026-11-06 (live step-output stream): the native tool lane
// is served over the provider's stream so its output can be forwarded live. These
// tests hold the two halves: the adapters emit tool-call fragments, and
// Provider.GenerateOnObserved keeps every GenerateOn guarantee (hooks on the
// complete text, the fallback walk) while reporting deltas.

const anthropicToolSSE = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-s\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n" +
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Done.\"}}\n\n" +
	"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"final_answer\",\"input\":{}}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"answer\\\": \\\"He\"}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"llo\\\"}\"}}\n\n" +
	"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":9}}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

func TestAnthropic_StreamEmitsToolCallFragments(t *testing.T) {
	t.Parallel()
	srv, _ := captureServer(t, anthropicToolSSE)
	c := &AnthropicClient{Endpoint: srv.URL, Model: "claude-x", NativeTools: true}
	ch, err := c.StreamRequest(context.Background(), domain.GenerateRequest{
		Messages: []domain.ModelMessage{domain.UserMessage("hi")},
		Tools:    []domain.ToolDefinition{{Name: "final_answer", Parameters: []byte(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var args strings.Builder
	var opened *domain.ToolCallDelta
	var final *domain.GenerateResponse
	for c := range ch {
		if c.ToolCall != nil {
			if c.Text != "" {
				t.Error("a tool-call fragment carries no text")
			}
			if c.ToolCall.Name != "" {
				opened = c.ToolCall
			}
			args.WriteString(c.ToolCall.ArgumentsDelta)
		}
		if c.Response != nil {
			final = c.Response
		}
	}
	if opened == nil || opened.Name != "final_answer" || opened.ID != "toolu_1" || opened.Index != 1 {
		t.Fatalf("opening fragment = %+v", opened)
	}
	if args.String() != `{"answer": "Hello"}` {
		t.Fatalf("argument fragments = %q", args.String())
	}
	if final == nil || len(final.ToolCalls) != 1 || string(final.ToolCalls[0].Arguments) != `{"answer": "Hello"}` {
		t.Fatalf("the final response must still carry the whole call: %+v", final)
	}
}

const openAIToolSSE = `data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m1","choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}

data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"final_answer","arguments":"{\"answer\":\"Wor"}}]},"finish_reason":null}]}

data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ld\"}"}}]},"finish_reason":"tool_calls"}]}

data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}

data: [DONE]

`

func TestOpenAI_StreamEmitsToolCallFragments(t *testing.T) {
	t.Parallel()
	srv, _ := captureServer(t, openAIToolSSE)
	c := &OpenAIClient{Endpoint: srv.URL, Model: "m", NativeTools: true}
	ch, err := c.StreamRequest(context.Background(), domain.GenerateRequest{
		Messages: []domain.ModelMessage{domain.UserMessage("hi")},
		Tools:    []domain.ToolDefinition{{Name: "final_answer", Parameters: []byte(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var frags []domain.ToolCallDelta
	for c := range ch {
		if c.ToolCall != nil {
			frags = append(frags, *c.ToolCall)
		}
	}
	if len(frags) != 2 || frags[0].Name != "final_answer" || frags[0].ID != "call_1" ||
		frags[0].ArgumentsDelta+frags[1].ArgumentsDelta != `{"answer":"World"}` || frags[1].Index != 0 {
		t.Fatalf("fragments = %+v", frags)
	}
}

// sseProvider is a one- or two-generator Provider over SSE servers, native tools on.
func sseProvider(t *testing.T, hooks *domain.HookChain, urls ...string) *Provider {
	t.Helper()
	var gens []config.GeneratorConfig
	for i, u := range urls {
		gens = append(gens, config.GeneratorConfig{ID: string(rune('a' + i)), Provider: "openai", Model: "m",
			Endpoint: u, TimeoutMs: 5000, NativeTools: true})
	}
	var opts []ProviderOption
	if hooks != nil {
		opts = append(opts, WithHooks(hooks))
	}
	p, err := NewProvider(config.LLMProviderConfig{
		Default:    "a",
		Generators: gens,
		Health:     config.HealthConfig{FailureThreshold: 5, CooldownMs: 60000},
	}, nil, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func toolReq() domain.GenerateRequest {
	req := domain.UserPrompt("answer")
	req.Tools = []domain.ToolDefinition{{Name: "final_answer", Parameters: []byte(`{"type":"object"}`)}}
	return req
}

func TestGenerateOnObserved_ReportsDeltasAndReturnsTheSameResponse(t *testing.T) {
	t.Parallel()
	srv, _ := captureServer(t, openAIToolSSE)
	p := sseProvider(t, nil, srv.URL)
	var deltas []domain.ModelStreamDelta
	resp, served, err := p.GenerateOnObserved(context.Background(), []string{"a"}, toolReq(), func(d domain.ModelStreamDelta) {
		deltas = append(deltas, d)
	})
	if err != nil || served != "a" {
		t.Fatalf("served %q, %v", served, err)
	}
	if len(deltas) != 3 || deltas[0].Text != "Hel" || deltas[1].ToolCall == nil || deltas[2].ToolCall == nil {
		t.Fatalf("deltas = %+v", deltas)
	}
	if resp.Text != "Hel" || len(resp.ToolCalls) != 1 || string(resp.ToolCalls[0].Arguments) != `{"answer":"World"}` ||
		resp.StopReason != domain.StopToolUse || resp.Usage.Input != 10 {
		t.Fatalf("response = %+v", resp)
	}
}

// rewriteHook is a may_rewrite post_model hook.
type rewriteHook struct{}

func (rewriteHook) Spec() domain.HookSpec {
	return domain.HookSpec{Name: "upper", Points: []domain.HookPoint{domain.HookPostModel}, MayRewrite: true}
}
func (rewriteHook) Run(_ context.Context, ev domain.HookEvent) (domain.HookResult, error) {
	up := strings.ToUpper(ev.Text)
	return domain.HookResult{Text: &up}, nil
}

func TestGenerateOnObserved_PostModelHookStillRunsOnTheCompleteText(t *testing.T) {
	t.Parallel()
	chain, err := domain.NewHookChain([]domain.Hook{rewriteHook{}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := captureServer(t, openAIToolSSE)
	p := sseProvider(t, chain, srv.URL)
	resp, _, err := p.GenerateOnObserved(context.Background(), []string{"a"}, toolReq(), func(domain.ModelStreamDelta) {})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "HEL" {
		t.Fatalf("post_model must govern what the caller receives: text = %q", resp.Text)
	}
}

// brokenStreamServer starts an SSE stream and then drops the connection.
func brokenStreamServer(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"x","object":"chat.completion.chunk","created":1,"model":"m1","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`+"\n\n")
		w.(http.Flusher).Flush()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGenerateOnObserved_MidStreamBreakFallsBackAndSaysRestarted(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	a := brokenStreamServer(t, &hits)
	b, _ := captureServer(t, openAIToolSSE)
	p := sseProvider(t, nil, a.URL, b.URL)
	var deltas []domain.ModelStreamDelta
	resp, served, err := p.GenerateOnObserved(context.Background(), []string{"a", "b"}, toolReq(), func(d domain.ModelStreamDelta) {
		deltas = append(deltas, d)
	})
	if err != nil || served != "b" {
		t.Fatalf("served %q, %v; a broken stream is an endpoint failure, the next candidate serves", served, err)
	}
	if len(deltas) < 2 || deltas[0].Text != "partial" || !deltas[1].Restarted {
		t.Fatalf("deltas = %+v; want a's partial text, then Restarted, then b's", deltas)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("response = %+v", resp)
	}
}

// unaryOnly is a RequestGenerator whose stream is unsupported.
type unaryOnly struct{ calls atomic.Int32 }

func (u *unaryOnly) GenerateRequest(context.Context, domain.GenerateRequest) (domain.GenerateResponse, error) {
	u.calls.Add(1)
	return domain.GenerateResponse{Text: "unary", StopReason: domain.StopEndTurn}, nil
}
func (u *unaryOnly) StreamRequest(context.Context, domain.GenerateRequest) (<-chan domain.StreamChunk, error) {
	return nil, domain.UnsupportedField("stream", "this fake cannot stream")
}
func (u *unaryOnly) Capabilities() domain.Capabilities { return domain.Capabilities{} }

func TestConsumeObservedStream_UnsupportedStreamServesUnaryWithNoDeltas(t *testing.T) {
	t.Parallel()
	u := &unaryOnly{}
	saw := 0
	resp, delta, err := consumeObservedStream(context.Background(), u, domain.UserPrompt("x"), func(domain.ModelStreamDelta) { saw++ })
	if err != nil || resp.Text != "unary" || delta || saw != 0 || u.calls.Load() != 1 {
		t.Fatalf("resp=%+v delta=%v err=%v saw=%d calls=%d", resp, delta, err, saw, u.calls.Load())
	}
}

func TestConsumeObservedStream_StreamErrorTextIsNeverADelta(t *testing.T) {
	t.Parallel()
	ch := make(chan domain.StreamChunk, 2)
	ch <- domain.StreamChunk{Text: "ok "}
	ch <- domain.StreamChunk{IsFinal: true, Text: "stream_error: boom"}
	close(ch)
	var got []string
	_, _, err := consumeObservedStream(context.Background(), chanGen{ch}, domain.UserPrompt("x"), func(d domain.ModelStreamDelta) {
		got = append(got, d.Text)
	})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	if strings.Join(got, "") != "ok " {
		t.Fatalf("the stream_error text leaked as a delta: %q", got)
	}
}

type chanGen struct{ ch chan domain.StreamChunk }

func (g chanGen) GenerateRequest(context.Context, domain.GenerateRequest) (domain.GenerateResponse, error) {
	return domain.GenerateResponse{}, errors.New("unused")
}
func (g chanGen) StreamRequest(context.Context, domain.GenerateRequest) (<-chan domain.StreamChunk, error) {
	return g.ch, nil
}
func (g chanGen) Capabilities() domain.Capabilities { return domain.Capabilities{} }
