package domain

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// fixedScanner returns a verdict keyed on a marker in the text.
type fixedScanner struct{}

func (fixedScanner) ScanContent(text string) ContentScan {
	switch {
	case strings.Contains(text, "EVIL"):
		return ContentScan{Verdict: ContentScanDangerous, Findings: []string{"PI-01 prompt_injection/danger line 1"}}
	case strings.Contains(text, "ODD"):
		return ContentScan{Verdict: ContentScanSuspicious, Findings: []string{"PI-05 prompt_injection/caution line 1"}}
	}
	return ContentScan{Verdict: ContentScanClean}
}

func TestNewUntrustedOutputGuard_Refuses(t *testing.T) {
	if _, err := NewUntrustedOutputGuard(nil, ContentScanDangerous, UntrustedHitFlag); err == nil {
		t.Error("nil scanner accepted")
	}
	if _, err := NewUntrustedOutputGuard(fixedScanner{}, ContentScanClean, UntrustedHitFlag); err == nil {
		t.Error("hit_at clean accepted")
	}
	if _, err := NewUntrustedOutputGuard(fixedScanner{}, ContentScanDangerous, "approve"); err == nil {
		t.Error("unknown action accepted")
	}
}

func TestUntrustedOutputGuard_Threshold(t *testing.T) {
	dangerOnly, _ := NewUntrustedOutputGuard(fixedScanner{}, ContentScanDangerous, UntrustedHitFlag)
	if u := dangerOnly.Inspect("t", []byte(`"ODD"`)); u.Hit || u.Scan.Verdict != ContentScanSuspicious {
		t.Fatalf("suspicious under hit_at dangerous: %+v", u)
	}
	if u := dangerOnly.Inspect("t", []byte(`"EVIL"`)); !u.Hit || dangerOnly.Withholds(u) {
		t.Fatalf("dangerous: %+v", u)
	}
	cautious, _ := NewUntrustedOutputGuard(fixedScanner{}, ContentScanSuspicious, UntrustedHitWithhold)
	if u := cautious.Inspect("t", []byte(`"ODD"`)); !u.Hit || !cautious.Withholds(u) {
		t.Fatalf("suspicious under hit_at suspicious, withhold: %+v", u)
	}
	var none *UntrustedOutputGuard
	if u := none.Inspect("t", []byte(`"EVIL"`)); u.Hit || u.Scan.Verdict != ContentScanNotScanned || none.Withholds(u) {
		t.Fatalf("nil guard: %+v", u)
	}
}

// The scan reads decoded strings: a JSON escape cannot hide a phrase.
func TestUntrustedText_DecodesJSON(t *testing.T) {
	got := UntrustedText([]byte(`{"b":[{"text":"line\nE\u0056IL"}],"a":"x"}`))
	if !strings.Contains(got, "line\nEVIL") || !strings.HasPrefix(got, "a\nx\nb") {
		t.Fatalf("UntrustedText = %q", got)
	}
	if UntrustedText([]byte("not json EVIL")) != "not json EVIL" {
		t.Fatal("non-JSON body not scanned as is")
	}
}

func TestAgentToolResult(t *testing.T) {
	u := &UntrustedOutput{Source: "mcp:s/t", Scan: ContentScan{Verdict: ContentScanClean}}
	// Unmarked: as it is.
	if r, e := AgentToolResult(ToolCallResponse{ResultJSON: []byte(`{"a":1}`), Error: "x"}); r != `{"a":1}` || e != "x" {
		t.Fatalf("unmarked changed: %s %s", r, e)
	}
	// Marked result: the envelope, the raw bytes inside the fence.
	r, _ := AgentToolResult(ToolCallResponse{ResultJSON: []byte(`{"a":1}`), Untrusted: u})
	var env map[string]any
	if err := json.Unmarshal([]byte(r), &env); err != nil || env["tool"] != "mcp:s/t" ||
		!strings.Contains(env["fenced_result"].(string), `"{\"a\":1}"`) || env["injection_scan"] == nil {
		t.Fatalf("fenced result: %s (%v)", r, err)
	}
	// Offloaded: the preview is fenced, the summary stays kernel-authored.
	s := OffloadSummary{Ref: "led_1", Preview: "EVIL preview", Read: "ledger_get id='led_1'"}
	r, _ = AgentToolResult(ToolCallResponse{ResultCID: "led_1", Offloaded: &s, Untrusted: u})
	if !strings.Contains(r, "PAYLOAD_FENCE_") || !strings.Contains(r, "ledger_get id='led_1'") || s.Preview != "EVIL preview" {
		t.Fatalf("offloaded: %s", r)
	}
	// A tool-reported error is fenced; a kernel error of a marked response is not.
	ue := *u
	ue.ErrorFromSource = true
	if _, e := AgentToolResult(ToolCallResponse{Error: "mcp tool \"t\" reported an error: EVIL", Untrusted: &ue}); !strings.Contains(e, "PAYLOAD_FENCE_") ||
		strings.Contains(strings.SplitN(e, "PAYLOAD_FENCE_", 2)[0], "EVIL") {
		t.Fatalf("tool error: %s", e)
	}
	if _, e := AgentToolResult(ToolCallResponse{Error: "kernel", Untrusted: u}); e != "kernel" {
		t.Fatalf("kernel error fenced: %s", e)
	}
}

// The mark survives the action-record JSON round trip, so an idempotent replay
// of a stored MCP response is fenced too.
func TestUntrustedOutput_SurvivesStoredResponse(t *testing.T) {
	b, _ := json.Marshal(ToolCallResponse{ResultJSON: []byte(`1`), Untrusted: &UntrustedOutput{Source: "mcp:s/t", Hit: true}})
	var back ToolCallResponse
	if err := json.Unmarshal(b, &back); err != nil || back.Untrusted == nil || back.Untrusted.Source != "mcp:s/t" || !back.Untrusted.Hit {
		t.Fatalf("round trip: %+v %v", back, err)
	}
}

type staticMCP struct {
	out []byte
	err error
}

func (h staticMCP) Execute(context.Context, ToolCall) ([]byte, error) { return h.out, h.err }

func untrustedExec(t *testing.T, h ToolHandler, action UntrustedHitAction) *ToolExecutor {
	t.Helper()
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "mcp:s/t"})
	reg.Register(SystemTool{Name: "native"})
	g, _ := NewUntrustedOutputGuard(fixedScanner{}, ContentScanDangerous, action)
	e, err := NewToolExecutor(ToolExecutorControls{Registry: reg, Grants: NewInMemoryGrantsStore(), Authz: AllowAllAuthorizer{},
		Unrestricted: true, UntrustedOutput: g}, ToolExecutor{MCPHandler: h, Handler: h})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestToolExecutor_MarksOnlyMCPAgentCalls(t *testing.T) {
	e := untrustedExec(t, staticMCP{out: []byte(`"EVIL"`)}, UntrustedHitFlag)
	ctx := context.Background()
	resp := e.Execute(ctx, ToolCallRequest{AgentID: "a", ToolName: "mcp:s/t"})
	if resp.Untrusted == nil || !resp.Untrusted.Hit || string(resp.ResultJSON) != `"EVIL"` || len(resp.Annotations) != 1 {
		t.Fatalf("MCP call: %+v", resp)
	}
	if r := e.Execute(ctx, ToolCallRequest{AgentID: "a", ToolName: "native"}); r.Untrusted != nil {
		t.Fatalf("native call marked: %+v", r)
	}
	if r := e.Execute(ctx, ToolCallRequest{AgentID: "a", ToolName: "mcp:s/t", System: true}); r.Untrusted != nil {
		t.Fatalf("operator System call marked: %+v", r)
	}
}

func TestToolExecutor_WithholdAndToolErrors(t *testing.T) {
	ctx := context.Background()
	w := untrustedExec(t, staticMCP{out: []byte(`"EVIL"`)}, UntrustedHitWithhold)
	resp := w.Execute(ctx, ToolCallRequest{AgentID: "a", ToolName: "mcp:s/t"})
	if resp.Denied || resp.Error != "" || resp.Untrusted != nil || strings.Contains(string(resp.ResultJSON), "EVIL") ||
		!strings.Contains(string(resp.ResultJSON), "output_withheld") {
		t.Fatalf("withheld: %+v", resp)
	}
	// A tool-reported error is marked; a transport error is not.
	f := untrustedExec(t, staticMCP{err: &ToolReportedError{Tool: "t", Message: "EVIL"}}, UntrustedHitFlag)
	if r := f.Execute(ctx, ToolCallRequest{AgentID: "a", ToolName: "mcp:s/t"}); r.Untrusted == nil || !r.Untrusted.ErrorFromSource || !strings.Contains(r.Error, "EVIL") {
		t.Fatalf("tool error: %+v", r)
	}
	k := untrustedExec(t, staticMCP{err: context.DeadlineExceeded}, UntrustedHitFlag)
	if r := k.Execute(ctx, ToolCallRequest{AgentID: "a", ToolName: "mcp:s/t"}); r.Untrusted != nil {
		t.Fatalf("transport error marked: %+v", r)
	}
	ww := untrustedExec(t, staticMCP{err: &ToolReportedError{Tool: "t", Message: "EVIL"}}, UntrustedHitWithhold)
	if r := ww.Execute(ctx, ToolCallRequest{AgentID: "a", ToolName: "mcp:s/t"}); strings.Contains(r.Error, "EVIL") || !strings.Contains(r.Error, "withheld") {
		t.Fatalf("withheld tool error: %+v", r)
	}
}

func TestNewToolExecutor_RefusesUntrustedOutputOnFeatures(t *testing.T) {
	g, _ := NewUntrustedOutputGuard(fixedScanner{}, ContentScanDangerous, UntrustedHitFlag)
	if _, err := NewToolExecutor(ToolExecutorControls{Registry: NewInMemoryToolRegistry(), Grants: NewInMemoryGrantsStore(), Authz: AllowAllAuthorizer{}},
		ToolExecutor{untrustedOutput: g}); err == nil {
		t.Fatal("a control set on features was accepted")
	}
}
