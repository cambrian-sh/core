package agentmgr

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// testHandler is a simple slog.Handler that captures log records for inspection.
type testHandler struct {
	mu      sync.Mutex
	records []slog.Record
	attrs   []map[string]any
}

func (h *testHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *testHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := make(map[string]any)
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.Any()
		return true
	})
	h.records = append(h.records, r)
	h.attrs = append(h.attrs, m)
	return nil
}

func (h *testHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return h }
func (h *testHandler) WithGroup(name string) slog.Handler       { return h }

// Cycle 3: forwardPipe forwards JSON lines with fields and plain lines as msg
func TestForwardPipe(t *testing.T) {
	handler := &testHandler{}
	origLogger := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(origLogger)

	t.Run("JSON line with msg field forwarded with fields", func(t *testing.T) {
		handler.records = nil
		handler.attrs = nil

		jsonLine := `{"msg":"hello from agent","level":"info","custom_field":"value123"}` + "\n"
		buf := bytes.NewBufferString(jsonLine)

		forwardPipe(t.Context(), buf, "agent-42", false)

		handler.mu.Lock()
		defer handler.mu.Unlock()

		if len(handler.records) != 1 {
			t.Fatalf("expected 1 log record, got %d", len(handler.records))
		}
		rec := handler.records[0]
		if rec.Message != "hello from agent" {
			t.Errorf("expected msg='hello from agent', got %q", rec.Message)
		}
		attrs := handler.attrs[0]
		if _, ok := attrs["agent_id"]; !ok {
			t.Error("expected agent_id attr in log record")
		}
		if attrs["agent_id"] != "agent-42" {
			t.Errorf("expected agent_id='agent-42', got %v", attrs["agent_id"])
		}
		if attrs["custom_field"] != "value123" {
			t.Errorf("expected custom_field='value123', got %v", attrs["custom_field"])
		}
		if rec.Level != slog.LevelInfo {
			t.Errorf("expected Info level for stdout, got %v", rec.Level)
		}
	})

	t.Run("plain text line forwarded as msg with agent_id", func(t *testing.T) {
		handler.records = nil
		handler.attrs = nil

		plainLine := "starting agent server on port 50053\n"
		buf := bytes.NewBufferString(plainLine)

		forwardPipe(t.Context(), buf, "agent-99", false)

		handler.mu.Lock()
		defer handler.mu.Unlock()

		if len(handler.records) != 1 {
			t.Fatalf("expected 1 log record, got %d", len(handler.records))
		}
		rec := handler.records[0]
		expected := strings.TrimRight(plainLine, "\n")
		if rec.Message != expected {
			t.Errorf("expected msg=%q, got %q", expected, rec.Message)
		}
		attrs := handler.attrs[0]
		if attrs["agent_id"] != "agent-99" {
			t.Errorf("expected agent_id='agent-99', got %v", attrs["agent_id"])
		}
		if rec.Level != slog.LevelInfo {
			t.Errorf("expected Info level for stdout, got %v", rec.Level)
		}
	})

	t.Run("stderr line uses Error level", func(t *testing.T) {
		handler.records = nil
		handler.attrs = nil

		buf := bytes.NewBufferString("something went wrong\n")
		forwardPipe(t.Context(), buf, "agent-err", true)

		handler.mu.Lock()
		defer handler.mu.Unlock()

		if len(handler.records) != 1 {
			t.Fatalf("expected 1 log record, got %d", len(handler.records))
		}
		rec := handler.records[0]
		if rec.Level != slog.LevelError {
			t.Errorf("expected Error level for stderr, got %v", rec.Level)
		}
	})

	t.Run("JSON stderr line uses Error level", func(t *testing.T) {
		handler.records = nil
		handler.attrs = nil

		jsonLine := `{"msg":"crash","reason":"nil pointer"}` + "\n"
		buf := bytes.NewBufferString(jsonLine)
		forwardPipe(t.Context(), buf, "agent-crash", true)

		handler.mu.Lock()
		defer handler.mu.Unlock()

		if len(handler.records) != 1 {
			t.Fatalf("expected 1 log record, got %d", len(handler.records))
		}
		rec := handler.records[0]
		if rec.Level != slog.LevelError {
			t.Errorf("expected Error level for stderr JSON, got %v", rec.Level)
		}
		if rec.Message != "crash" {
			t.Errorf("expected msg='crash', got %q", rec.Message)
		}
		attrs := handler.attrs[0]
		if attrs["reason"] != "nil pointer" {
			t.Errorf("expected reason='nil pointer', got %v", attrs["reason"])
		}
	})

	t.Run("multiple lines processed", func(t *testing.T) {
		handler.records = nil
		handler.attrs = nil

		input := `{"msg":"line1","val":"a"}` + "\n" + "plain line2\n"
		buf := bytes.NewBufferString(input)
		forwardPipe(t.Context(), buf, "agent-multi", false)

		handler.mu.Lock()
		defer handler.mu.Unlock()

		if len(handler.records) != 2 {
			t.Fatalf("expected 2 log records, got %d", len(handler.records))
		}
		if handler.records[0].Message != "line1" {
			t.Errorf("first record msg: expected 'line1', got %q", handler.records[0].Message)
		}
		if handler.records[1].Message != "plain line2" {
			t.Errorf("second record msg: expected 'plain line2', got %q", handler.records[1].Message)
		}
	})
}
