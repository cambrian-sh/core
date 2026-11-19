package domain

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// ENG-531: the Rule-of-Two gate and the content provenance it reads.

func TestRecordSourceClass_FromKernelStampedProvenance(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]any
		tags []string
		want SourceClass
	}{
		{"source principal stamp", nil, []string{"crm", ProvenanceSourceTagPrefix + "source:slack:ws"}, SourceIngested},
		{"ingest thread", map[string]any{MetaIngestThreadID: "thr-1"}, nil, SourceIngested},
		{"corpus chunk", map[string]any{"document_id": "doc-9"}, nil, SourceIngested},
		{"agent stamp", nil, []string{ProvenanceSourceTagPrefix + "planner"}, SourceOtherAgent},
		{"nothing known", map[string]any{}, nil, SourceOtherAgent},
	}
	for _, c := range cases {
		if got := RecordSourceClass(c.meta, c.tags); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestToolResultSourceClass_ByEffects(t *testing.T) {
	if c := ToolResultSourceClass(SystemTool{Effects: []ToolEffect{EffectRead, EffectEgressRead}}); c != SourceWeb {
		t.Errorf("egress_read tool: %q", c)
	}
	if c := ToolResultSourceClass(SystemTool{Effects: []ToolEffect{EffectRead}}); c != SourceTool {
		t.Errorf("read tool: %q", c)
	}
	if c := ToolResultSourceClass(SystemTool{}); c != SourceWeb {
		t.Errorf("no effects must assume the widest reach: %q", c)
	}
}

func TestNewTaintScope_KeysFromBindingLeaseOnlyAsLastResort(t *testing.T) {
	s := NewTaintScope("L1", LeaseBinding{SessionID: "S1", RunID: "R1"}, "", "C1")
	if got := strings.Join(s.Keys(), ","); got != "conversation:C1,run:R1,session:S1" {
		t.Fatalf("keys = %s", got)
	}
	if got := NewTaintScope("L1", LeaseBinding{}, "", "").Keys(); len(got) != 1 || got[0] != "lease:L1" {
		t.Fatalf("a bare lease is its own session: %v", got)
	}
	if !NewTaintScope("", LeaseBinding{}, "", "").IsZero() {
		t.Fatal("no identity must be the zero scope")
	}
}

func TestProvenanceTracker_UnionAcrossKeysAndExpiry(t *testing.T) {
	now := time.Unix(0, 0)
	tr := NewProvenanceTracker(time.Hour, func() time.Time { return now })
	step := NewTaintScope("L1", LeaseBinding{SessionID: "S1"}, "", "C1")
	turn := NewTaintScope("L2", LeaseBinding{ConversationID: "C1"}, "", "")
	tr.Mark(step, SourceIngested, []string{"pii"})
	if st := tr.State(turn); len(st.Sources) != 1 || st.Sources[0] != SourceIngested || len(st.SensitiveTags) != 1 {
		t.Fatalf("the conversation's next turn must see the step's taint: %+v", st)
	}
	now = now.Add(2 * time.Hour)
	if st := tr.State(turn); len(st.Sources) != 0 {
		t.Fatalf("expired taint still reported: %+v", st)
	}
	tr.Mark(TaintScope{}, SourceWeb, nil) // zero scope records nothing
	if len(tr.entries) != 2 {
		t.Fatalf("entries = %d", len(tr.entries))
	}
}

func TestNewRuleOfTwoGate_RefusesAnInertOrUnknownPolicy(t *testing.T) {
	tr := NewProvenanceTracker(0, nil)
	ok := RuleOfTwoPolicy{UntrustedSources: []SourceClass{SourceWeb}, GatedEffects: []ToolEffect{EffectEgressSend}}
	for name, c := range map[string]struct {
		p  RuleOfTwoPolicy
		tr *ProvenanceTracker
	}{
		"nil tracker":     {ok, nil},
		"no sources":      {RuleOfTwoPolicy{GatedEffects: ok.GatedEffects}, tr},
		"no effects":      {RuleOfTwoPolicy{UntrustedSources: ok.UntrustedSources}, tr},
		"unknown source":  {RuleOfTwoPolicy{UntrustedSources: []SourceClass{"email"}, GatedEffects: ok.GatedEffects}, tr},
		"unknown effects": {RuleOfTwoPolicy{UntrustedSources: ok.UntrustedSources, GatedEffects: []ToolEffect{"delete"}}, tr},
	} {
		if _, err := NewRuleOfTwoGate(c.p, c.tr); !errors.Is(err, ErrRuleOfTwoPolicy) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := NewRuleOfTwoGate(ok, tr); err != nil {
		t.Fatal(err)
	}
}

func newGate(t *testing.T, sensitive ...string) *RuleOfTwoGate {
	t.Helper()
	g, err := NewRuleOfTwoGate(RuleOfTwoPolicy{
		UntrustedSources: []SourceClass{SourceWeb, SourceTool, SourceOtherAgent, SourceIngested},
		SensitiveTags:    sensitive,
		GatedEffects:     []ToolEffect{EffectWrite, EffectEgressSend, EffectSpend, EffectAdmin},
	}, NewProvenanceTracker(0, nil))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

var (
	sendTool = SystemTool{Name: "mail_send", Effects: []ToolEffect{EffectRead, EffectEgressSend}}
	readTool = SystemTool{Name: "crm_read", Effects: []ToolEffect{EffectRead}, ClassificationTags: []string{"crm"}}
	webRead  = SystemTool{Name: "web_search", Effects: []ToolEffect{EffectRead, EffectEgressRead}}
)

func TestRuleOfTwoGate_AllThreeLegsRequired(t *testing.T) {
	g := newGate(t) // no sensitivity vocabulary ⇒ every session is sensitive
	scope := NewTaintScope("L", LeaseBinding{SessionID: "S"}, "", "")
	if _, hit := g.Check(scope, sendTool, nil); hit {
		t.Fatal("an untainted session must not be gated")
	}
	g.Observe(scope, SourceUser, nil)
	if _, hit := g.Check(scope, sendTool, nil); hit {
		t.Fatal("trusted (user) content must not taint")
	}
	g.Observe(scope, SourceIngested, nil)
	hit, gated := g.Check(scope, sendTool, nil)
	if !gated || len(hit.Sources) != 1 || hit.Sources[0] != SourceIngested || !hit.AllSensitive {
		t.Fatalf("tainted ∧ sensitive ∧ egress_send must be gated: %+v %v", hit, gated)
	}
	if _, gated := g.Check(scope, readTool, nil); gated {
		t.Fatal("a read-only call is never gated")
	}
	if _, gated := g.Check(scope, webRead, nil); gated {
		t.Fatal("egress_read is not a gated effect by default")
	}
	if !strings.Contains(RenderRuleOfTwoNotice(hit), "ingested") {
		t.Fatalf("notice must name the source: %q", RenderRuleOfTwoNotice(hit))
	}
}

func TestRuleOfTwoGate_ZeroScopeIsTaintedFailClosed(t *testing.T) {
	g := newGate(t)
	hit, gated := g.Check(TaintScope{}, sendTool, nil)
	if !gated || !hit.UnknownProvenance {
		t.Fatalf("unknown provenance must count as tainted: %+v", hit)
	}
	if _, gated := g.Check(TaintScope{}, SystemTool{Name: "x"}, nil); !gated {
		t.Fatal("a tool with no effects must be gated (fail closed)")
	}
}

func TestRuleOfTwoGate_SensitiveLegFromRecordsGrantsOrUnreadableGrants(t *testing.T) {
	g := newGate(t, "pii")
	scope := NewTaintScope("L", LeaseBinding{SessionID: "S"}, "", "")
	g.Observe(scope, SourceWeb, []string{"public"})
	noGrants := func() ([]string, error) { return []string{"crm"}, nil }
	if _, gated := g.Check(scope, sendTool, noGrants); gated {
		t.Fatal("tainted but no sensitive access: two legs only, not gated")
	}
	// A sensitive grant completes it.
	hit, gated := g.Check(scope, sendTool, func() ([]string, error) { return []string{"pii"}, nil })
	if !gated || len(hit.SensitiveTags) != 1 || hit.SensitiveTags[0] != "pii" {
		t.Fatalf("sensitive grant: %+v %v", hit, gated)
	}
	// An unreadable grant store is sensitive (fail closed).
	if hit, gated := g.Check(scope, sendTool, func() ([]string, error) { return nil, errors.New("down") }); !gated || !hit.SensitiveUnknown {
		t.Fatalf("grant error must count as sensitive: %+v", hit)
	}
	// Reading a sensitive record completes it without any grant.
	g.Observe(scope, SourceIngested, []string{"pii", "hr"})
	if _, gated := g.Check(scope, sendTool, noGrants); !gated {
		t.Fatal("a session that read a pii record holds sensitive access")
	}
}

// ruleOfTwoExec builds the executor through NewToolExecutor with the gate as a
// constructor control.
func ruleOfTwoExec(t *testing.T, appr ApprovalController, h ToolHandler, g *RuleOfTwoGate) *ToolExecutor {
	t.Helper()
	reg := NewInMemoryToolRegistry()
	reg.Register(sendTool)
	reg.Register(webRead)
	reg.Register(readTool)
	grants := grantStore("a", ToolGrant{Tool: "mail_send"}, ToolGrant{Tool: "web_search"}, ToolGrant{Tool: "crm_read"})
	var ctl ApprovalController
	if appr != nil {
		ctl = appr
	}
	e, err := NewToolExecutor(ToolExecutorControls{Registry: reg, Grants: grants, Authz: AllowAllAuthorizer{},
		Approval: ctl, RuleOfTwo: g}, ToolExecutor{Handler: h, InlineThreshold: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestNewToolExecutor_RuleOfTwoOnlyThroughControls(t *testing.T) {
	_, err := NewToolExecutor(ToolExecutorControls{Registry: NewInMemoryToolRegistry(), Grants: NewInMemoryGrantsStore(), Authz: AllowAllAuthorizer{}},
		ToolExecutor{ruleOfTwo: newGate(t)})
	if !errors.Is(err, ErrToolExecutorControl) {
		t.Fatalf("a gate set on features must be refused: %v", err)
	}
}

func TestExecute_WebResultTaintsThenSendGoesToApprovalWithNotice(t *testing.T) {
	appr := &recordingApproval{approve: true}
	h := &fakeHandler{result: []byte(`{"ok":true}`)}
	g := newGate(t)
	e := ruleOfTwoExec(t, appr, h, g)
	ctx := WithTaintScope(context.Background(), NewTaintScope("L", LeaseBinding{SessionID: "S"}, "", ""))

	// Untainted: the send runs with no approval.
	if out := e.Execute(ctx, ToolCallRequest{AgentID: "a", ToolName: "mail_send", ArgsJSON: []byte(`{"to":"x"}`)}); out.Denied || len(appr.reqs) != 0 {
		t.Fatalf("untainted send must run unasked: %+v approvals=%d", out, len(appr.reqs))
	}
	// A web read: its result is web content the session consumed.
	out := e.Execute(ctx, ToolCallRequest{AgentID: "a", ToolName: "web_search", ArgsJSON: []byte(`{"q":"x"}`)})
	if out.Denied || out.SourceClass != SourceWeb || len(appr.reqs) != 0 {
		t.Fatalf("web read: %+v approvals=%d", out, len(appr.reqs))
	}
	// A read-only call is still not gated.
	if out := e.Execute(ctx, ToolCallRequest{AgentID: "a", ToolName: "crm_read", ArgsJSON: []byte(`{}`)}); out.Denied || len(appr.reqs) != 0 {
		t.Fatalf("read-only call gated: %+v", out)
	}
	// Now the send asks, through the one approval path, with the notice.
	if out := e.Execute(ctx, ToolCallRequest{AgentID: "a", ToolName: "mail_send", ArgsJSON: []byte(`{"to":"x"}`)}); out.Denied {
		t.Fatalf("approved send denied: %+v", out)
	}
	if len(appr.reqs) != 1 || !strings.Contains(appr.reqs[0].DuplicateNotice, "Rule of Two") ||
		!strings.Contains(appr.reqs[0].DuplicateNotice, "web") || appr.reqs[0].Intent == nil {
		t.Fatalf("approval request: %+v", appr.reqs)
	}
	// Another session is untouched.
	other := WithTaintScope(context.Background(), NewTaintScope("L2", LeaseBinding{SessionID: "S2"}, "", ""))
	e.Execute(other, ToolCallRequest{AgentID: "a", ToolName: "mail_send", ArgsJSON: []byte(`{"to":"y"}`)})
	if len(appr.reqs) != 1 {
		t.Fatal("taint leaked into another session")
	}
}

func TestExecute_RuleOfTwoWithoutApprovalControllerIsDenied(t *testing.T) {
	h := &fakeHandler{result: []byte(`{"ok":true}`)}
	g := newGate(t)
	e := ruleOfTwoExec(t, nil, h, g)
	scope := NewTaintScope("L", LeaseBinding{SessionID: "S"}, "", "")
	g.Observe(scope, SourceIngested, nil)
	out := e.Execute(WithTaintScope(context.Background(), scope), ToolCallRequest{AgentID: "a", ToolName: "mail_send", ArgsJSON: []byte(`{}`)})
	if !out.Denied || h.called || !strings.Contains(out.DenyReason, "Rule of Two") {
		t.Fatalf("fail closed: %+v called=%v", out, h.called)
	}
	// An operator ScopeSystem call carries its own authority.
	h.called = false
	if out := e.Execute(context.Background(), ToolCallRequest{System: true, ToolName: "mail_send", ArgsJSON: []byte(`{}`)}); out.Denied || !h.called {
		t.Fatalf("system call gated: %+v", out)
	}
}

type sourcedKernelTool struct {
	name  string
	class SourceClass
}

func (k sourcedKernelTool) Spec() SystemTool { return SystemTool{Name: k.name} }
func (k sourcedKernelTool) Call(context.Context, KernelToolCall) ([]byte, error) {
	return []byte(`{}`), nil
}
func (k sourcedKernelTool) ResultSource() SourceClass { return k.class }

func TestExecute_KernelToolResultsReportTheirOwnSource(t *testing.T) {
	g := newGate(t)
	e := ruleOfTwoExec(t, &recordingApproval{approve: true}, &fakeHandler{}, g)
	e.KernelTools = []KernelTool{sourcedKernelTool{"calc_x", ""}, sourcedKernelTool{"records_x", SourceIngested}}
	scope := NewTaintScope("L", LeaseBinding{SessionID: "S"}, "", "")
	ctx := WithTaintScope(context.Background(), scope)
	e.Execute(ctx, ToolCallRequest{AgentID: "a", ToolName: "calc_x"})
	if st := g.State(scope); len(st.Sources) != 0 {
		t.Fatalf("a computation adds no content: %+v", st)
	}
	out := e.Execute(ctx, ToolCallRequest{AgentID: "a", ToolName: "records_x"})
	if st := g.State(scope); len(st.Sources) != 1 || st.Sources[0] != SourceIngested || out.SourceClass != SourceIngested {
		t.Fatalf("records result must be ingested content: %+v %+v", st, out)
	}
}
