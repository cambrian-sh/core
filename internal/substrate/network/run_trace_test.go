package network

import (
	"context"
	"sync"
	"testing"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

type memRunTracer struct {
	mu    sync.Mutex
	spans []domain.TraceSpan
}

func (m *memRunTracer) Span(_ context.Context, s domain.TraceSpan) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spans = append(m.spans, s)
}

// ADR-0145 D10: a kernel-executed tool call is recorded with the model's exact
// arguments and the result the agent received.
func TestTraceToolCall(t *testing.T) {
	tr := &memRunTracer{}
	s := &Server{RunTracer: tr}
	req := &pb.ExecuteToolRequest{ToolName: "mcp:fs/read_file", ArgsJson: `{"path":"a.txt"}`}
	s.traceToolCall(t.Context(), "", time.Now(), "worker-1", "lease-1", req,
		domain.ToolCallResponse{ResultJSON: []byte(`{"text":"hello"}`), Error: ""})
	s.traceToolCall(t.Context(), "", time.Now(), "worker-1", "lease-1", req,
		domain.ToolCallResponse{Error: "no such file"})
	if len(tr.spans) != 2 {
		t.Fatalf("spans = %d", len(tr.spans))
	}
	a := tr.spans[0].Attributes
	if tr.spans[0].Name != "execute_tool mcp:fs/read_file" || a[domain.GenAIToolCallArguments] != `{"path":"a.txt"}` ||
		a[domain.GenAIToolCallResult] != `{"text":"hello"}` || a[domain.GenAIAgentName] != "worker-1" {
		t.Errorf("span: %s %v", tr.spans[0].Name, a)
	}
	if tr.spans[1].Error != "no such file" {
		t.Errorf("error span: %+v", tr.spans[1])
	}
	(&Server{}).traceToolCall(t.Context(), "", time.Now(), "", "", req, domain.ToolCallResponse{}) // nil tracer: no panic
}

// The start event of a tool call carries the arguments and no result, and the
// finished span completes it under the same span id; no tracer writes nothing.
func TestTraceToolCallStart(t *testing.T) {
	tr := &memRunTracer{}
	s := &Server{RunTracer: tr}
	req := &pb.ExecuteToolRequest{ToolName: "mcp:fs/read_file", ArgsJson: `{"path":"a.txt"}`}
	id, start := s.traceToolCallStart(t.Context(), "worker-1", "lease-1", req)
	if len(tr.spans) != 1 {
		t.Fatalf("spans after start = %d", len(tr.spans))
	}
	st := tr.spans[0]
	if st.Phase != domain.TracePhaseStart || st.SpanID != id || id == "" ||
		st.Attributes[domain.GenAIToolCallArguments] != `{"path":"a.txt"}` || st.Attributes["cambrian.lease.id"] != "lease-1" {
		t.Errorf("start event: %+v", st)
	}
	if _, has := st.Attributes[domain.GenAIToolCallResult]; has {
		t.Error("a start event has no result")
	}
	s.traceToolCall(t.Context(), id, start, "worker-1", "lease-1", req, domain.ToolCallResponse{ResultJSON: []byte(`"x"`)})
	if end := tr.spans[1]; end.SpanID != id || end.Phase != "" || !end.Start.Equal(start) {
		t.Errorf("end span: %+v", end)
	}
	if id2, _ := (&Server{}).traceToolCallStart(t.Context(), "", "", req); id2 != "" {
		t.Errorf("no tracer must mint no span: %q", id2)
	}
}

// The gateway names who a model call is for on the run trace: the lease, the bound
// agent, the run and step, the conversation (ctx attributes the Provider's trace
// layer cannot know).
func TestLeaseTraceAttrs(t *testing.T) {
	a := leaseTraceAttrs("lease-1", domain.LeaseBinding{AgentID: "composed_worker", RunID: "r1", StepIndex: 2, ConversationID: "c1"})
	if a["cambrian.lease.id"] != "lease-1" || a[domain.GenAIAgentName] != "composed_worker" ||
		a["cambrian.run.id"] != "r1" || a["cambrian.step.index"] != 2 || a[domain.GenAIConversationID] != "c1" {
		t.Errorf("attrs: %v", a)
	}
	if b := leaseTraceAttrs("lease-2", domain.LeaseBinding{StepIndex: -1}); len(b) != 1 {
		t.Errorf("an unbound lease names only itself: %v", b)
	}
}
