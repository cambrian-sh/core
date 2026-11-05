package domain

import (
	"context"
	"strings"
	"testing"
)

type fakeToolOutputRecorder struct{ got []ToolOutputRecord }

func (f *fakeToolOutputRecorder) RecordToolOutput(_ context.Context, rec ToolOutputRecord) error {
	f.got = append(f.got, rec)
	return nil
}

// ADR-0049 D1: the action formatter renders a mutation into a compact deterministic
// line; large arg values collapse, status reflects the outcome.
func TestFormatActionLine(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", 200)
	line := FormatActionLine("write_file", []byte(`{"path":"a.md","content":"`+big+`"}`), []byte(`{"message":"ok"}`))
	if !strings.HasPrefix(line, "write_file → ok") {
		t.Errorf("expected 'write_file → ok' prefix; got %q", line)
	}
	if !strings.Contains(line, "path=a.md") {
		t.Errorf("small arg must stay verbatim; got %q", line)
	}
	if strings.Contains(line, big) || !strings.Contains(line, "<200 chars>") {
		t.Errorf("large arg must collapse to a marker; got %q", line)
	}
	if s := FormatActionLine("rm", []byte(`{"path":"x"}`), []byte(`{"error":"perm"}`)); !strings.Contains(s, "→ error") {
		t.Errorf("error status expected; got %q", s)
	}
	if s := FormatActionLine("rm", nil, []byte(`{"denied":true}`)); !strings.Contains(s, "→ denied") {
		t.Errorf("denied status expected; got %q", s)
	}
	// A failed-shell shape reads as error (reuses the ADR-0048 #3 detector).
	if s := FormatActionLine("terminal", []byte(`{"cmd":"x"}`), []byte(`{"exit_code":1,"stderr":"boom","stdout":""}`)); !strings.Contains(s, "→ error") {
		t.Errorf("failed shell status expected; got %q", s)
	}
}

// Every non-denied tool call is reported to the recorder, with its read/write nature;
// the recorder decides what to keep (since 2026-11-05: mutations only).
func TestToolExecutor_ReportsEveryNonDeniedCall(t *testing.T) {
	t.Parallel()
	run := func(result []byte) []ToolOutputRecord {
		reg := NewInMemoryToolRegistry()
		reg.Register(SystemTool{Name: "web_search"}) // a read tool (no DataWriteKinds)
		rec := &fakeToolOutputRecorder{}
		e := newExec(reg, grantStore("a", ToolGrant{Tool: "web_search"}), &fakeHandler{result: result})
		e.ToolOutput = rec
		e.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "web_search"})
		return rec.got
	}
	if got := run([]byte(`{"result":"` + strings.Repeat("x", 300) + `"}`)); len(got) != 1 || got[0].ToolName != "web_search" || got[0].IsMutation {
		t.Errorf("a read output must be reported as a non-mutation, got %+v", got)
	}
	if got := run([]byte(`{"r":"small"}`)); len(got) != 1 {
		t.Errorf("a small output must be reported too, got %+v", got)
	}
	if got := run([]byte(`{"denied":true}`)); len(got) != 0 {
		t.Errorf("a denied call never ran and must not be reported, got %+v", got)
	}
}
