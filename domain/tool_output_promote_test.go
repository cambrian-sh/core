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

// The pure cost pre-filter: below the floor, error, and denied are skipped;
// a substantive non-error output is promoted.
func TestShouldPromoteToolOutput(t *testing.T) {
	pad := strings.Repeat("x", 300)
	if shouldPromoteToolOutput([]byte("tiny"), 200) {
		t.Error("below the size floor must be skipped")
	}
	if !shouldPromoteToolOutput([]byte(`{"result":"`+pad+`"}`), 200) {
		t.Error("a substantive non-error output must promote")
	}
	if shouldPromoteToolOutput([]byte(`{"error":"`+pad+`"}`), 200) {
		t.Error("an error result must be skipped")
	}
	if shouldPromoteToolOutput([]byte(`{"denied":true,"p":"`+pad+`"}`), 200) {
		t.Error("a denied result must be skipped")
	}
	// ADR-0048 #3: shell-failure shapes are skipped at the raw result (the
	// "tool[name]:" envelope would later defeat Tier-2's JSON error detection).
	if shouldPromoteToolOutput([]byte(`{"exit_code":2,"stdout":"","stderr":"`+pad+`"}`), 200) {
		t.Error("a non-zero exit_code must be skipped")
	}
	if shouldPromoteToolOutput([]byte(`{"stdout":"","stderr":"ps: unknown option `+pad+`"}`), 200) {
		t.Error("stderr with empty stdout (the ps failure shape) must be skipped")
	}
	// A result with real stdout is kept even if it also emits a stderr warning.
	if !shouldPromoteToolOutput([]byte(`{"exit_code":0,"stdout":"`+pad+`","stderr":"warn"}`), 200) {
		t.Error("substantive stdout must promote despite a stderr warning")
	}
}

// ADR-0049 D1: the action formatter renders a mutation into a compact deterministic
// line; large arg values collapse, status reflects the outcome.
func TestFormatActionLine(t *testing.T) {
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

// A successful, substantive tool output is fed to the recorder; an error output
// (handler returns a result containing an error marker) is not.
func TestToolExecutor_PromotesSubstantiveOutputOnly(t *testing.T) {
	pad := strings.Repeat("x", 300)

	run := func(result []byte) []ToolOutputRecord {
		reg := NewInMemoryToolRegistry()
		reg.Register(SystemTool{Name: "web_search"}) // a read tool (no DataWriteKinds)
		rec := &fakeToolOutputRecorder{}
		e := newExec(reg, grantStore("a", ToolGrant{Tool: "web_search"}), &fakeHandler{result: result})
		e.ToolOutput = rec
		e.ToolOutputMinBytes = 200
		e.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "web_search"})
		return rec.got
	}

	if got := run([]byte(`{"result":"` + pad + `"}`)); len(got) != 1 || got[0].ToolName != "web_search" || got[0].IsMutation {
		t.Errorf("substantive read output should be recorded as a non-mutation, got %v", got)
	}
	if got := run([]byte(`{"error":"` + pad + `"}`)); len(got) != 0 {
		t.Errorf("error output must not be recorded, got %v", got)
	}
	if got := run([]byte(`{"r":"small"}`)); len(got) != 0 {
		t.Errorf("below-floor output must not be recorded, got %v", got)
	}
}
