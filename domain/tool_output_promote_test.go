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

// actionStatus reads a tool result's outcome in-band (it gates which results the
// LTM pre-filter keeps): denied, error, a failed shell shape, else ok.
func TestActionStatus(t *testing.T) {
	t.Parallel()
	for out, want := range map[string]string{
		`{"message":"ok"}`: "ok",
		`{"error":"perm"}`: "error",
		`{"denied":true}`:  "denied",
		`{"exit_code":1,"stderr":"boom","stdout":""}`: "error",
		`not json`: "ok",
	} {
		if got := actionStatus([]byte(out)); got != want {
			t.Errorf("actionStatus(%s) = %q, want %q", out, got, want)
		}
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
