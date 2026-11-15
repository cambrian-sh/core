package domain

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The outbound identifier guard (ENG-917). These tests use a small fake detector
// (domain cannot import internal/identifiers); the real detector runs end to end
// in app/outbound_identifiers_test.go.

var fakeNationalID = regexp.MustCompile(`\b\d{11}\b`)

type fakeDetector struct{}

func (fakeDetector) FindIdentifiers(text string) []IdentifierMatch {
	var out []IdentifierMatch
	for _, m := range fakeNationalID.FindAllStringIndex(text, -1) {
		out = append(out, IdentifierMatch{Start: m[0], End: m[1], Kind: IdentifierNationalID, Value: text[m[0]:m[1]]})
	}
	return out
}

func newGuard(t *testing.T) *OutboundIdentifierGuard {
	t.Helper()
	g, err := NewOutboundIdentifierGuard(fakeDetector{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestSendsOutside_ByDeclaredEffects(t *testing.T) {
	for _, tc := range []struct {
		name string
		tool SystemTool
		want bool
	}{
		{"egress_send", SystemTool{Effects: []ToolEffect{EffectWrite, EffectEgressSend}}, true},
		{"plain egress counts as send", SystemTool{Effects: []ToolEffect{EffectRead, EffectEgress}}, true},
		{"egress_read is not guarded", SystemTool{Effects: []ToolEffect{EffectRead, EffectEgressRead}}, false},
		{"read only", SystemTool{Effects: []ToolEffect{EffectRead}}, false},
		{"inferred egress counts as send", SystemTool{Effects: []ToolEffect{EffectRead, EffectEgress}, EffectsInferred: true}, true},
		{"no effects fails closed", SystemTool{}, true},
		{"undeclared MCP tool (read+write+egress)", SystemTool{Dangerous: true, Effects: []ToolEffect{EffectRead, EffectWrite, EffectEgress}}, true},
	} {
		if got := SendsOutside(tc.tool); got != tc.want {
			t.Errorf("%s: SendsOutside = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestOutboundIdentifierGuard_ScanNamesTheArgument(t *testing.T) {
	g := newGuard(t)
	got := g.Scan([]byte(`{"to":["a@b.c"],"letter":{"body":"TC 10304949101 and 55879350776"},"n":10304949101}`))
	if len(got) != 2 || got[0].Arg != "letter.body" || got[0].Value != "10304949101" || got[1].Value != "55879350776" {
		t.Fatalf("scan = %+v (numbers are not text and must not be scanned)", got)
	}
	if raw := g.Scan([]byte(`not json 10304949101`)); len(raw) != 1 || raw[0].Arg != "" {
		t.Fatalf("non-JSON args must be scanned as one text: %+v", raw)
	}
	notice := RenderOutboundIdentifierNotice(got)
	if !strings.HasPrefix(notice, OutboundIdentifierNoticePrefix) || !strings.Contains(notice, "«10304949101» in letter.body") {
		t.Fatalf("notice = %q", notice)
	}
}

func TestNewOutboundIdentifierGuard_RefusesUnknownKindAndNilDetector(t *testing.T) {
	if _, err := NewOutboundIdentifierGuard(nil, nil); err == nil {
		t.Fatal("nil detector accepted")
	}
	if _, err := NewOutboundIdentifierGuard(fakeDetector{}, []IdentifierKind{"passport"}); err == nil {
		t.Fatal("unknown kind accepted")
	}
	g, err := NewOutboundIdentifierGuard(fakeDetector{}, []IdentifierKind{IdentifierPhone})
	if err != nil {
		t.Fatal(err)
	}
	if got := g.Scan([]byte(`{"t":"10304949101"}`)); len(got) != 0 {
		t.Fatalf("a kind outside the configured set counted: %+v", got)
	}
}

func TestNewToolExecutor_GuardOnlyThroughControls(t *testing.T) {
	_, err := NewToolExecutor(ToolExecutorControls{Registry: NewInMemoryToolRegistry(), Grants: NewInMemoryGrantsStore(), Authz: AllowAllAuthorizer{}},
		ToolExecutor{outboundIdentifiers: newGuard(t)})
	if !errors.Is(err, ErrToolExecutorControl) {
		t.Fatalf("a guard set on features must be refused, got %v", err)
	}
}

// outboundExec builds the executor through NewToolExecutor with the guard as a
// constructor control, over three tools: a non-dangerous egress_send tool, an
// egress_read tool and an undeclared (fail-closed) one.
func outboundExec(t *testing.T, appr ApprovalController, h ToolHandler) *ToolExecutor {
	t.Helper()
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "cases_respond_request", Effects: []ToolEffect{EffectWrite, EffectEgressSend}})
	reg.Register(SystemTool{Name: "web_search", Effects: []ToolEffect{EffectRead, EffectEgressRead}})
	reg.Register(SystemTool{Name: "undeclared", Dangerous: true, Effects: []ToolEffect{EffectRead, EffectWrite, EffectEgress}})
	grants := grantStore("a", ToolGrant{Tool: "cases_respond_request"}, ToolGrant{Tool: "web_search"}, ToolGrant{Tool: "undeclared"})
	e, err := NewToolExecutor(ToolExecutorControls{Registry: reg, Grants: grants, Authz: AllowAllAuthorizer{},
		Approval: appr, OutboundIdentifiers: newGuard(t)}, ToolExecutor{Handler: h, InlineThreshold: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

const letterWithID = `{"case":"BE-2026/0001","letter":"Dear applicant, the record of Merve (TC 10304949101) is released."}`

func TestExecute_EgressSendWithIdentifierGoesToApprovalWithHighlights(t *testing.T) {
	appr := &recordingApproval{approve: false}
	h := &fakeHandler{result: []byte(`{"ok":true}`)}
	e := outboundExec(t, appr, h)
	out := e.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "cases_respond_request", ArgsJSON: []byte(letterWithID)})
	if !out.Denied || h.called {
		t.Fatalf("a refused approval must stop the call: %+v called=%v", out, h.called)
	}
	if len(appr.reqs) != 1 {
		t.Fatalf("approvals raised = %d, want 1", len(appr.reqs))
	}
	r := appr.reqs[0]
	if !strings.Contains(r.DuplicateNotice, OutboundIdentifierNoticePrefix) || !strings.Contains(r.DuplicateNotice, "«10304949101» in letter") {
		t.Fatalf("approver notice = %q", r.DuplicateNotice)
	}
	if len(r.PersonalIdentifiers) != 1 || r.PersonalIdentifiers[0].Value != "10304949101" {
		t.Fatalf("request identifiers = %+v", r.PersonalIdentifiers)
	}
	if !strings.Contains(string(r.Args), "10304949101") {
		t.Fatal("the call must never be rewritten or masked")
	}

	// Approved, the call runs with its arguments exactly as written.
	appr.approve = true
	out = e.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "cases_respond_request", ArgsJSON: []byte(letterWithID)})
	if out.Denied || !h.called || string(h.gotArgs) != letterWithID {
		t.Fatalf("approved call: %+v args=%s", out, h.gotArgs)
	}
}

func TestExecute_EgressSendWithoutIdentifierNeedsNoApproval(t *testing.T) {
	appr := &recordingApproval{approve: false}
	h := &fakeHandler{result: []byte(`{"ok":true}`)}
	e := outboundExec(t, appr, h)
	out := e.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "cases_respond_request",
		ArgsJSON: []byte(`{"case":"BE-2026/0001","letter":"Dear applicant, the records are released with personal data removed."}`)})
	if out.Denied || !h.called || len(appr.reqs) != 0 {
		t.Fatalf("no identifier ⇒ no extra approval: %+v called=%v approvals=%d", out, h.called, len(appr.reqs))
	}
}

func TestExecute_EgressReadWithIdentifierIsNotGuarded(t *testing.T) {
	appr := &recordingApproval{approve: false}
	h := &fakeHandler{result: []byte(`{"hits":[]}`)}
	e := outboundExec(t, appr, h)
	out := e.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "web_search", ArgsJSON: []byte(`{"q":"10304949101"}`)})
	if out.Denied || !h.called || len(appr.reqs) != 0 {
		t.Fatalf("egress_read must not be guarded: %+v approvals=%d", out, len(appr.reqs))
	}
}

func TestExecute_UndeclaredToolApprovalCarriesHighlights(t *testing.T) {
	appr := &recordingApproval{approve: true}
	h := &fakeHandler{result: []byte(`{"ok":true}`)}
	e := outboundExec(t, appr, h)
	// Without an identifier: the usual dangerous-tool approval, no identifier notice.
	e.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "undeclared", ArgsJSON: []byte(`{"x":"hello"}`)})
	// With one: one approval (not two), carrying the identifiers.
	e.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "undeclared", ArgsJSON: []byte(`{"x":"id 55879350776"}`)})
	if len(appr.reqs) != 2 {
		t.Fatalf("approvals = %d, want one per call", len(appr.reqs))
	}
	if strings.Contains(appr.reqs[0].DuplicateNotice, OutboundIdentifierNoticePrefix) {
		t.Fatalf("no identifier, yet a notice: %q", appr.reqs[0].DuplicateNotice)
	}
	if !strings.Contains(appr.reqs[1].DuplicateNotice, "«55879350776» in x") {
		t.Fatalf("undeclared tool's approval lacks the highlight: %q", appr.reqs[1].DuplicateNotice)
	}
}

func TestExecute_GuardHitWithoutApprovalControllerIsDenied(t *testing.T) {
	h := &fakeHandler{result: []byte(`{"ok":true}`)}
	e := outboundExec(t, nil, h)
	out := e.Execute(context.Background(), ToolCallRequest{AgentID: "a", ToolName: "cases_respond_request", ArgsJSON: []byte(letterWithID)})
	if !out.Denied || h.called || !strings.Contains(out.DenyReason, "approval required but unavailable") ||
		!strings.Contains(out.DenyReason, "10304949101") {
		t.Fatalf("fail closed: %+v called=%v", out, h.called)
	}
}

func TestExecute_OperatorSystemCallIsNotGuarded(t *testing.T) {
	h := &fakeHandler{result: []byte(`{"ok":true}`)}
	e := outboundExec(t, nil, h)
	out := e.Execute(context.Background(), ToolCallRequest{System: true, ToolName: "cases_respond_request", ArgsJSON: []byte(letterWithID)})
	if out.Denied || !h.called {
		t.Fatalf("an operator ScopeSystem call carries its own authority: %+v", out)
	}
}

// A standing approval never pre-approves a call that carries personal identifiers,
// and the answer to such a request cannot grant one.
func TestHub_PersonalIdentifiersAreNeverStandingApproved(t *testing.T) {
	h := NewAddressedHub(AddressedHubConfig{ConsentTimeout: 2 * time.Second})
	ch, cancel := h.Watch(opWatch("op"))
	defer cancel()
	ids := []OutboundIdentifier{{Arg: "letter", Kind: IdentifierNationalID, Value: "10304949101"}}
	req := func(withIDs bool) ApprovalRequest {
		r := ApprovalRequest{AgentID: "a", ToolName: "cases_respond_request", ArgsPreview: "{}", Timeout: 2 * time.Second}
		if withIDs {
			r.PersonalIdentifiers = ids
		}
		return r
	}
	// An identifier request cannot be answered with a standing approval.
	done := raiseAsync(context.Background(), h, req(true))
	it := nextRaised(t, ch)
	if _, err := h.Answer(context.Background(), it.ID, ApprovalAnswer{Approved: true, Standing: true, By: OperatorApprover("op")}); !errors.Is(err, ErrStandingNotAllowed) {
		t.Fatalf("standing answer on an identifier request: err=%v", err)
	}
	_, _ = h.Answer(context.Background(), it.ID, ApprovalAnswer{Approved: true, By: OperatorApprover("op")})
	<-done
	// A plain request may grant a standing approval for the tool ...
	done = raiseAsync(context.Background(), h, req(false))
	it = nextRaised(t, ch)
	if _, err := h.Answer(context.Background(), it.ID, ApprovalAnswer{Approved: true, Standing: true, StandingTTL: time.Hour, By: OperatorApprover("op")}); err != nil {
		t.Fatal(err)
	}
	<-done
	if d, _ := h.Request(context.Background(), req(false)); !d.Approved || !strings.HasPrefix(d.ApproverID, "standing:") {
		t.Fatalf("plain request should be standing-approved: %+v", d)
	}
	// ... but it never covers a call carrying identifiers: a person is asked.
	done = raiseAsync(context.Background(), h, req(true))
	it = nextRaised(t, ch)
	if it.AllowStanding {
		t.Fatal("identifier item must not allow standing")
	}
	_, _ = h.Answer(context.Background(), it.ID, ApprovalAnswer{Approved: false, By: OperatorApprover("op")})
	if d := <-done; d.Approved {
		t.Fatalf("identifier call approved without a person: %+v", d)
	}
}
