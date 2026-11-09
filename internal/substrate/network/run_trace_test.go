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
	s.traceToolCall(t.Context(), time.Now(), "worker-1", "lease-1", req,
		domain.ToolCallResponse{ResultJSON: []byte(`{"text":"hello"}`), Error: ""})
	s.traceToolCall(t.Context(), time.Now(), "worker-1", "lease-1", req,
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
	(&Server{}).traceToolCall(t.Context(), time.Now(), "", "", req, domain.ToolCallResponse{}) // nil tracer: no panic
}
