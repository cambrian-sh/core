package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// toolCallServer returns a stub /chat/completions that answers with the given
// finish_reason and (optionally) one tool call, in the exact shape the live endpoint
// was verified to produce on 2026-07-28 — arguments as a JSON *string*, not an object.
func toolCallServer(t *testing.T, finishReason string, withCall bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		msg := map[string]any{"content": "sure"}
		if withCall {
			msg["tool_calls"] = []map[string]any{{
				"id":   "call_abc123",
				"type": "function",
				"function": map[string]any{
					"name":      "write_file",
					"arguments": `{"path":"./x.md","content":"hi"}`,
				},
			}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"finish_reason": finishReason, "message": msg}},
		})
	}))
}

func TestOpenAI_GenerateWithTools_ParsesCall(t *testing.T) {
	srv := toolCallServer(t, "tool_calls", true)
	defer srv.Close()
	c := &OpenAIClient{Endpoint: srv.URL, Model: "m", TimeoutMs: 5000, NativeTools: true}

	turn, err := c.GenerateWithTools(context.Background(), []domain.ModelMessage{domain.UserMessage("write a file")}, []domain.ToolDefinition{
		{Name: "write_file", Description: "write", Parameters: []byte(`{"type":"object"}`)},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if turn.StopReason != domain.StopToolUse {
		t.Errorf("StopReason = %q, want %q", turn.StopReason, domain.StopToolUse)
	}
	if len(turn.ToolCalls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(turn.ToolCalls))
	}
	tc := turn.ToolCalls[0]
	// The provider id must survive verbatim: it is what correlates the result, and a
	// locally-generated one is rejected.
	if tc.ID != "call_abc123" {
		t.Errorf("tool call id = %q, want it preserved verbatim", tc.ID)
	}
	if tc.Name != "write_file" {
		t.Errorf("tool name = %q", tc.Name)
	}
	var args map[string]string
	if err := json.Unmarshal(tc.Arguments, &args); err != nil {
		t.Fatalf("arguments must be forwarded as raw JSON bytes: %v", err)
	}
	if args["path"] != "./x.md" {
		t.Errorf("arguments not preserved: %v", args)
	}
	if !turn.ShouldContinue() {
		t.Error("a turn carrying a tool call must continue the loop")
	}
}

func TestOpenAI_GenerateWithTools_SendsToolsInRequest(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"finish_reason": "stop", "message": map[string]any{"content": "hi"}}},
		})
	}))
	defer srv.Close()
	c := &OpenAIClient{Endpoint: srv.URL, Model: "m", TimeoutMs: 5000, NativeTools: true}

	if _, err := c.GenerateWithTools(context.Background(), []domain.ModelMessage{domain.UserMessage("hi")}, []domain.ToolDefinition{
		{Name: "write_file", Parameters: []byte(`{"type":"object"}`)},
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tools, _ := got["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools must reach the wire, got %v", got["tools"])
	}
	if got["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %v, want auto", got["tool_choice"])
	}
}

// Sending tool_choice with no tools is rejected by some gateways, so it must be
// omitted rather than sent alongside an empty list.
func TestOpenAI_GenerateWithTools_NoToolsOmitsToolChoice(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"finish_reason": "stop", "message": map[string]any{"content": "hi"}}},
		})
	}))
	defer srv.Close()
	c := &OpenAIClient{Endpoint: srv.URL, Model: "m", TimeoutMs: 5000, NativeTools: true}

	if _, err := c.GenerateWithTools(context.Background(), []domain.ModelMessage{domain.UserMessage("hi")}, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, present := got["tool_choice"]; present {
		t.Error("tool_choice must be omitted when no tools are offered")
	}
}

func TestNormalizeOpenAIFinishReason(t *testing.T) {
	cases := map[string]domain.StopReason{
		"tool_calls":     domain.StopToolUse,
		"function_call":  domain.StopToolUse,
		"stop":           domain.StopEndTurn,
		"length":         domain.StopMaxTokens,
		"content_filter": domain.StopRefusal,
		"":               domain.StopUnknown,
		"something_new":  domain.StopUnknown,
	}
	for in, want := range cases {
		if got := normalizeOpenAIFinishReason(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
	// An unmapped provider value must not read as finished.
	if normalizeOpenAIFinishReason("brand_new_reason").IsFinished() {
		t.Fatal("an unmapped finish_reason must never be treated as finished")
	}
}

// ── D2: the capability is declared, and absent when it is not ────────────────

func TestFactory_NativeToolsDeclared_ExposesCapability(t *testing.T) {
	gen, _, err := NewClient(config.ModelConfig{
		Provider: "openai", Model: "m", Endpoint: "http://x", NativeTools: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !domain.SupportsToolCalling(gen) {
		t.Fatal("native_tools=true must expose the tool-calling capability")
	}
}

// The important direction: an endpoint that has NOT declared support must not merely
// refuse at call time — the capability must be structurally absent, or
// SupportsToolCalling lies and callers branch onto a path that then fails.
func TestFactory_NativeToolsUndeclared_HidesCapability(t *testing.T) {
	gen, _, err := NewClient(config.ModelConfig{
		Provider: "openai", Model: "m", Endpoint: "http://x"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if domain.SupportsToolCalling(gen) {
		t.Fatal("native_tools unset must hide the capability, not defer the failure")
	}
}

// ── D1: decorator forwarding ─────────────────────────────────────────────────

type fakeToolGen struct {
	calls int
	turn  domain.ModelTurn
}

func (f *fakeToolGen) Generate(context.Context, string) (string, error) { return "text", nil }

func (f *fakeToolGen) GenerateWithTools(context.Context, []domain.ModelMessage, []domain.ToolDefinition) (domain.ModelTurn, error) {
	f.calls++
	return f.turn, nil
}

type plainGen struct{}

func (plainGen) Generate(context.Context, string) (string, error) { return "text", nil }

// A decorator that forwards Generate but not GenerateWithTools silently downgrades a
// capable generator to the text path — no error, no log. That is the same shape as the
// EFE task rebuild dropping RequiredCapabilities and base.think() shadowing the
// tool-round default, each of which cost a day. One test per decorator.
func TestDecorators_ForwardToolCalling(t *testing.T) {
	inner := &fakeToolGen{turn: domain.ModelTurn{
		StopReason: domain.StopToolUse,
		ToolCalls:  []domain.ModelToolCall{{ID: "c1", Name: "write_file"}},
	}}

	decorators := map[string]domain.Generator{
		"healthGenerator":      newHealthGenerator("g1", inner, NewCircuitBreaker(3, time.Minute)),
		"concurrencyGenerator": &concurrencyGenerator{inner: inner, sem: make(chan struct{}, 1)},
	}

	for name, dec := range decorators {
		t.Run(name, func(t *testing.T) {
			if !domain.SupportsToolCalling(dec) {
				t.Fatalf("%s must forward the tool-calling capability", name)
			}
			before := inner.calls
			turn, err := dec.(domain.ToolCallingGenerator).
				GenerateWithTools(context.Background(), []domain.ModelMessage{domain.UserMessage("go")}, nil)
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", name, err)
			}
			if inner.calls != before+1 {
				t.Fatalf("%s did not reach the inner generator", name)
			}
			if len(turn.ToolCalls) != 1 || turn.ToolCalls[0].ID != "c1" {
				t.Fatalf("%s mangled the forwarded turn: %+v", name, turn)
			}
		})
	}
}

// Forwarding must fail loudly when the inner cannot do it — not return an empty turn
// that a caller would read as "the model said nothing and finished".
func TestDecorators_PlainInnerErrorsRatherThanFakingATurn(t *testing.T) {
	decorators := map[string]domain.ToolCallingGenerator{
		"healthGenerator": newHealthGenerator("g1", plainGen{},
			NewCircuitBreaker(3, time.Minute)).(domain.ToolCallingGenerator),
		"concurrencyGenerator": &concurrencyGenerator{inner: plainGen{}, sem: make(chan struct{}, 1)},
	}
	for name, dec := range decorators {
		t.Run(name, func(t *testing.T) {
			turn, err := dec.GenerateWithTools(context.Background(), []domain.ModelMessage{domain.UserMessage("go")}, nil)
			if err == nil {
				t.Fatalf("%s must error when the inner lacks the capability", name)
			}
			if turn.StopReason.IsFinished() {
				t.Fatalf("%s returned a turn that reads as finished on failure", name)
			}
		})
	}
}

// A caller that reaches GenerateWithTools without checking SupportsToolCalling must
// get a loud error, not a silent plain completion that never calls anything.
func TestOpenAI_GenerateWithTools_RefusesWhenNotDeclared(t *testing.T) {
	c := &OpenAIClient{Endpoint: "http://x", Model: "m", TimeoutMs: 5000}
	if _, err := c.GenerateWithTools(context.Background(), []domain.ModelMessage{domain.UserMessage("hi")}, nil); err == nil {
		t.Fatal("must refuse when native_tools is not declared")
	}
	if domain.SupportsToolCalling(c) {
		t.Fatal("an undeclared client must report no tool-calling support")
	}
}

// REGRESSION: the first implementation hid the capability by wrapping the client in a
// value exposing only domain.Generator. That also hid STREAMING — embedding an
// interface narrows the method set to exactly that interface — so every OpenAI
// generator without native_tools would have silently lost streaming. Both capabilities
// must survive independently of each other.
func TestOpenAI_CapabilitiesAreIndependent(t *testing.T) {
	for _, native := range []bool{false, true} {
		gen, _, err := NewClient(config.ModelConfig{
			Provider: "openai", Model: "m", Endpoint: "http://x", NativeTools: native})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := gen.(streamingInner); !ok {
			t.Errorf("native_tools=%v: streaming must survive regardless of tool support", native)
		}
		if got := domain.SupportsToolCalling(gen); got != native {
			t.Errorf("native_tools=%v: SupportsToolCalling = %v", native, got)
		}
	}
}

// A decorator must not claim tool support on behalf of an inner that has it disabled.
func TestDecorators_DoNotOverReportDisabledInner(t *testing.T) {
	disabled := &OpenAIClient{Endpoint: "http://x", Model: "m"} // NativeTools false
	decs := map[string]domain.Generator{
		"healthGenerator":      newHealthGenerator("g1", disabled, NewCircuitBreaker(3, time.Minute)),
		"concurrencyGenerator": &concurrencyGenerator{inner: disabled, sem: make(chan struct{}, 1)},
	}
	for name, d := range decs {
		if domain.SupportsToolCalling(d) {
			t.Errorf("%s over-reports tool support for a disabled inner", name)
		}
	}
}

// End-to-end through the config plumbing: GeneratorConfig → ModelConfig → client →
// decorator chain → SupportsToolCalling.
//
// This exists because the capability crosses TWO field-by-field struct copies
// (NewGeneratorRegistry and NewProviderRegistryFromGenerators), and a field omitted
// from either is dropped with no compiler error. That exact shape has cost this
// codebase a day more than once — the EFE task rebuild, the shadowed tool-round
// default — so the wiring is asserted rather than assumed.
func TestGeneratorRegistry_PropagatesNativeTools(t *testing.T) {
	reg, err := NewGeneratorRegistry([]config.GeneratorConfig{
		{ID: "native", Provider: "openai", Model: "m", Endpoint: "http://x", NativeTools: true},
		{ID: "plain", Provider: "openai", Model: "m", Endpoint: "http://x"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for id, want := range map[string]bool{"native": true, "plain": false} {
		entry, ok := reg.entries[id]
		if !ok {
			t.Fatalf("generator %q missing from registry", id)
		}
		if got := domain.SupportsToolCalling(entry.Generator); got != want {
			t.Errorf("generator %q: SupportsToolCalling = %v, want %v", id, got, want)
		}
	}
}

// ADR-0097 D8: the wire must carry assistant tool_calls and tool results, or the model
// cannot see its own call. Sending one user turn per round is what made the first cut
// re-explore every turn and never write a file.
func TestOpenAI_SendsFullConversation(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"finish_reason": "stop", "message": map[string]any{"content": "ok"}}},
		})
	}))
	defer srv.Close()
	c := &OpenAIClient{Endpoint: srv.URL, Model: "m", TimeoutMs: 5000, NativeTools: true}

	_, err := c.GenerateWithTools(context.Background(), []domain.ModelMessage{
		domain.UserMessage("write it"),
		{Role: domain.RoleAssistant, ToolCalls: []domain.ModelToolCall{
			{ID: "call_1", Name: "write_file", Arguments: []byte(`{"path":"x"}`)}}},
		{Role: domain.RoleTool, ToolCallID: "call_1", Content: `{"ok":true}`},
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msgs, _ := got["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages on the wire, got %d", len(msgs))
	}
	asst, _ := msgs[1].(map[string]any)
	calls, _ := asst["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("assistant turn must carry tool_calls, got %v", asst)
	}
	call, _ := calls[0].(map[string]any)
	if call["id"] != "call_1" || call["type"] != "function" {
		t.Errorf("malformed tool_call on the wire: %v", call)
	}
	fn, _ := call["function"].(map[string]any)
	// Arguments go back as the provider's own JSON STRING, not re-encoded.
	if fn["arguments"] != `{"path":"x"}` {
		t.Errorf("arguments = %v, want the original string", fn["arguments"])
	}
	toolMsg, _ := msgs[2].(map[string]any)
	if toolMsg["tool_call_id"] != "call_1" || toolMsg["role"] != "tool" {
		t.Errorf("tool turn malformed: %v", toolMsg)
	}
}

// A plain text completion must not gain empty tool fields — the omitempty tags keep
// the non-tool path byte-identical to before ADR-0097.
func TestOpenAI_TextMessagesOmitToolFields(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"finish_reason": "stop", "message": map[string]any{"content": "ok"}}},
		})
	}))
	defer srv.Close()
	c := &OpenAIClient{Endpoint: srv.URL, Model: "m", TimeoutMs: 5000, NativeTools: true}

	if _, err := c.GenerateWithTools(context.Background(),
		[]domain.ModelMessage{domain.UserMessage("hi")}, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msgs, _ := got["messages"].([]any)
	m0, _ := msgs[0].(map[string]any)
	if _, present := m0["tool_calls"]; present {
		t.Error("a user turn must not carry an empty tool_calls field")
	}
	if _, present := m0["tool_call_id"]; present {
		t.Error("a user turn must not carry an empty tool_call_id field")
	}
}
