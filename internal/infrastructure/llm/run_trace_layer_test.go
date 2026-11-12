package llm

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// memTracer collects spans in memory.
type memTracer struct {
	mu    sync.Mutex
	spans []domain.TraceSpan
}

func (m *memTracer) Span(_ context.Context, s domain.TraceSpan) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spans = append(m.spans, s)
}

// all returns the finished spans (start events skipped).
func (m *memTracer) all() []domain.TraceSpan {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.TraceSpan
	for _, s := range m.spans {
		if s.Phase == "" {
			out = append(out, s)
		}
	}
	return out
}

// raw returns every recorded span and start event in order.
func (m *memTracer) raw() []domain.TraceSpan {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]domain.TraceSpan(nil), m.spans...)
}

// ADR-0145 D10: a model call through the Provider is recorded with what the model
// was sent (system, messages, tools offered) and what it answered.
func TestRunTrace_RecordsRequestAndResponse(t *testing.T) {
	t.Parallel()
	tr := &memTracer{}
	inner := &scriptedRG{replies: []domain.GenerateResponse{{
		Text: "calling", StopReason: domain.StopToolUse, Model: "haiku-x",
		ToolCalls: []domain.ModelToolCall{{ID: "c1", Name: "lookup", Arguments: []byte(`{"id":"INV-7"}`)}},
		Usage:     domain.GenerateUsage{Input: 11, Output: 3},
	}}}
	g := &runTraceGenerator{inner: inner, tracer: tr, id: "gen-1", purpose: "planner"}
	req := domain.GenerateRequest{
		System:   []domain.SystemBlock{{Text: "be exact"}},
		Messages: []domain.ModelMessage{domain.UserMessage("find INV-7")},
		Tools:    []domain.ToolDefinition{{Name: "lookup", Description: "find", Parameters: []byte(`{"type":"object"}`)}},
	}
	if _, err := g.GenerateRequest(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	spans := tr.all()
	if len(spans) != 1 {
		t.Fatalf("spans = %d", len(spans))
	}
	s := spans[0]
	a := s.Attributes
	if s.Name != "chat gen-1" || s.Kind != "CLIENT" || a[domain.GenAIOperationName] != "chat" || a["cambrian.purpose"] != "planner" {
		t.Errorf("span identity: %q %q %v", s.Name, s.Kind, a)
	}
	if _, ok := a[domain.GenAISystemInstructions]; !ok {
		t.Error("system instructions missing")
	}
	if names, _ := a["cambrian.tools.offered"].([]string); len(names) != 1 || names[0] != "lookup" {
		t.Errorf("tools offered: %v", a["cambrian.tools.offered"])
	}
	in := a[domain.GenAIInputMessages].([]map[string]any)
	if len(in) != 1 || in[0]["role"] != "user" {
		t.Errorf("input messages: %v", in)
	}
	out := a[domain.GenAIOutputMessages].([]map[string]any)
	parts := out[0]["parts"].([]map[string]any)
	if len(parts) != 2 || parts[1]["type"] != "tool_call" || parts[1]["arguments"] != `{"id":"INV-7"}` {
		t.Errorf("output parts: %v", parts)
	}
	if a[domain.GenAIUsageInputTokens] != 11 || a[domain.GenAIResponseModel] != "haiku-x" {
		t.Errorf("usage/model: %v %v", a[domain.GenAIUsageInputTokens], a[domain.GenAIResponseModel])
	}
}

// A streamed call is recorded once, when the stream ends, with the final response.
func TestRunTrace_StreamRecordedAtEnd(t *testing.T) {
	t.Parallel()
	tr := &memTracer{}
	g := &runTraceGenerator{inner: &scriptedRG{}, tracer: tr, id: "gen-1", purpose: "agent"}
	ch, err := g.StreamRequest(context.Background(), domain.UserPrompt("go"))
	if err != nil {
		t.Fatal(err)
	}
	drain(t, ch)
	spans := tr.all()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	out := spans[0].Attributes[domain.GenAIOutputMessages].([]map[string]any)
	if parts := out[0]["parts"].([]map[string]any); len(parts) != 1 || parts[0]["content"] != "default" {
		t.Errorf("stream output: %v", parts)
	}
}

// Wired through the Provider: SetRunTracer puts the layer on every acquired
// generator; without it nothing is recorded.
func TestProvider_RunTracerWrapsAcquire(t *testing.T) {
	t.Parallel()
	srv := respondServer(t, http.StatusOK, okBody(`"ok"`, "stop"))
	p := httpProvider(t, srv.srv.URL, "openai", 0, 0)
	tr := &memTracer{}
	p.SetRunTracer(tr)
	gen, err := p.Acquire(context.Background(), domain.LLMRequest{Purpose: domain.PurposePlanner})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gen.Generate(context.Background(), "plan this"); err != nil {
		t.Fatal(err)
	}
	if n := len(tr.all()); n != 1 {
		t.Fatalf("spans = %d, want 1", n)
	}
	if got := tr.all()[0].Attributes["cambrian.purpose"]; got != string(domain.PurposePlanner) {
		t.Errorf("purpose = %v", got)
	}
}

// T-20261109-live-run-viewer: a model call writes a start event with the request
// BEFORE the model answers, and the finished span carries the same span id.
func TestRunTrace_StartEventBeforeCall(t *testing.T) {
	t.Parallel()
	tr := &memTracer{}
	var atCall []domain.TraceSpan
	inner := &hookRG{onCall: func() { atCall = tr.raw() }}
	g := &runTraceGenerator{inner: inner, tracer: tr, id: "gen-1", purpose: "planner"}
	req := domain.GenerateRequest{
		System:         []domain.SystemBlock{{Text: "be exact"}},
		Messages:       []domain.ModelMessage{domain.UserMessage("find INV-7")},
		Tools:          []domain.ToolDefinition{{Name: "lookup", Parameters: []byte(`{"type":"object"}`)}},
		ResponseSchema: &domain.ResponseSchema{Name: "checklist_system", Schema: []byte(`{}`)},
	}
	if _, err := g.GenerateRequest(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(atCall) != 1 || atCall[0].Phase != domain.TracePhaseStart {
		t.Fatalf("while the model ran the trace held %d events, want the start event: %+v", len(atCall), atCall)
	}
	st := atCall[0]
	if st.Name != "chat gen-1" || st.SpanID == "" || st.Attributes["cambrian.purpose"] != "planner" ||
		st.Attributes["cambrian.request.response_schema_name"] != "checklist_system" {
		t.Errorf("start event: %s %q %v", st.Name, st.SpanID, st.Attributes)
	}
	if _, ok := st.Attributes[domain.GenAISystemInstructions]; !ok {
		t.Error("start event must carry the system instructions")
	}
	if _, ok := st.Attributes[domain.GenAIOutputMessages]; ok {
		t.Error("start event must carry no output")
	}
	all := tr.raw()
	if len(all) != 2 || all[1].SpanID != st.SpanID || all[1].Phase != "" {
		t.Errorf("end span must complete the pair: %+v", all)
	}
}

// A stream's start event is written when the stream opens, its end when it ends.
func TestRunTrace_StreamStartEvent(t *testing.T) {
	t.Parallel()
	tr := &memTracer{}
	g := &runTraceGenerator{inner: &scriptedRG{}, tracer: tr, id: "gen-1", purpose: "agent"}
	ch, err := g.StreamRequest(context.Background(), domain.UserPrompt("go"))
	if err != nil {
		t.Fatal(err)
	}
	if r := tr.raw(); len(r) != 1 || r[0].Phase != domain.TracePhaseStart {
		t.Fatalf("after open: %+v", r)
	}
	drain(t, ch)
	if r := tr.raw(); len(r) != 2 || r[0].SpanID != r[1].SpanID {
		t.Errorf("after end: %+v", r)
	}
}

// hookRG is a RequestGenerator that runs onCall while "the model" answers.
type hookRG struct {
	scriptedRG
	onCall func()
}

func (h *hookRG) GenerateRequest(ctx context.Context, req domain.GenerateRequest) (domain.GenerateResponse, error) {
	h.onCall()
	return h.scriptedRG.GenerateRequest(ctx, req)
}
