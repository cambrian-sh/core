package runtrace

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

func readSpans(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line is not JSON: %v", err)
		}
		out = append(out, m)
	}
	return out
}

func TestWriter_SpanShapeBoundsAndDedupe(t *testing.T) {
	w, err := Open(t.TempDir(), "cambrian-kernel", 10)
	if err != nil {
		t.Fatal(err)
	}
	ctx := domain.WithSessionID(t.Context(), "ses-1")
	sys := []map[string]any{{"type": "text", "content": "you are a careful planner"}}
	for i := 0; i < 2; i++ {
		w.Span(ctx, domain.TraceSpan{
			Name: "chat haiku", Kind: "CLIENT", Start: time.Unix(1, 0), End: time.Unix(2, 0),
			Attributes: map[string]any{
				domain.GenAIOperationName:      domain.GenAIOpChat,
				domain.GenAISystemInstructions: sys,
				domain.GenAIInputMessages:      []map[string]any{domain.GenAITextMessage("user", "a very long question indeed")},
			},
		})
	}
	w.Span(ctx, domain.TraceSpan{Name: "execute_tool x", Error: "boom"})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	spans := readSpans(t, w.Path())
	if len(spans) != 3 {
		t.Fatalf("spans = %d, want 3", len(spans))
	}
	a0 := spans[0]["attributes"].(map[string]any)
	a1 := spans[1]["attributes"].(map[string]any)
	if _, ok := a0[domain.GenAISystemInstructions]; !ok {
		t.Error("first span must carry the system instructions in full")
	}
	if _, ok := a1[domain.GenAISystemInstructions]; ok {
		t.Error("a repeated system prompt must be written by hash only")
	}
	if a0[domain.GenAISystemInstructions+"_sha256"] == "" || a0[domain.GenAISystemInstructions+"_sha256"] != a1[domain.GenAISystemInstructions+"_sha256"] {
		t.Error("both spans must name the same system hash")
	}
	msg, _ := json.Marshal(a0[domain.GenAIInputMessages])
	if !strings.Contains(string(msg), "truncated") {
		t.Errorf("long content not bounded: %s", msg)
	}
	if spans[0]["trace_id"] != spans[2]["trace_id"] || len(spans[0]["trace_id"].(string)) != 32 {
		t.Errorf("one session must share one 32-hex trace id: %v %v", spans[0]["trace_id"], spans[2]["trace_id"])
	}
	if a0["cambrian.session.id"] != "ses-1" {
		t.Errorf("session id not stamped: %v", a0["cambrian.session.id"])
	}
	if st := spans[2]["status"].(map[string]any); st["code"] != "ERROR" || st["message"] != "boom" {
		t.Errorf("error status: %v", st)
	}
}

func TestWriter_ConcurrentSpansAreWholeLines(t *testing.T) {
	w, err := Open(t.TempDir(), "k", 0)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.Span(t.Context(), domain.TraceSpan{Name: "chat m", Attributes: map[string]any{"x": strings.Repeat("y", 5000)}})
		}()
	}
	wg.Wait()
	_ = w.Close()
	if n := len(readSpans(t, w.Path())); n != 20 {
		t.Fatalf("lines = %d, want 20", n)
	}
}
