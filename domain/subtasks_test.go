package domain

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// ADR-0150 D2/D5/D9: the subtask effect ceiling, the egress split, labelled
// approvals and resume without a double write
// (T-20261113-adr-0150-slice-2-run-subtasks-kernel-tool-brief).

func TestEgressSplit_CoversExpandsAndInfersAsSend(t *testing.T) {
	if !EffectCovers(EffectEgress, EffectEgressRead) || !EffectCovers(EffectEgress, EffectEgressSend) {
		t.Fatal("a grant of plain egress must cover both halves")
	}
	if EffectCovers(EffectEgressRead, EffectEgress) || EffectCovers(EffectEgressRead, EffectEgressSend) {
		t.Fatal("egress_read must not cover send or plain egress")
	}
	got := ExpandEffects([]ToolEffect{EffectEgress, EffectRead})
	if len(got) != 3 || got[0] != EffectRead || got[1] != EffectEgressRead || got[2] != EffectEgressSend {
		t.Fatalf("ExpandEffects = %v", got)
	}
	for _, e := range []ToolEffect{EffectEgressRead, EffectEgressSend} {
		if !ValidToolEffect(e) {
			t.Fatalf("%s must be in the closed set", e)
		}
	}
	inferred, err := ValidateRegistration(SystemTool{Name: "fetch", URLArgs: []string{"url"}}, false)
	if err != nil || !inferred.EffectsInferred {
		t.Fatalf("expected inferred effects: %+v %v", inferred, err)
	}
	ce := CeilingEffects(inferred)
	for _, e := range ce {
		if e == EffectEgressRead {
			t.Fatalf("inferred egress must never count as egress_read: %v", ce)
		}
	}
	if b := EffectsBeyond(inferred, []ToolEffect{EffectRead, EffectEgressRead}); len(b) != 1 || b[0] != EffectEgressSend {
		t.Fatalf("inferred egress must be treated as send: beyond = %v", b)
	}
}

type labelApproval struct {
	mu   sync.Mutex
	reqs []ApprovalRequest
}

func (a *labelApproval) Request(_ context.Context, r ApprovalRequest) (ApprovalDecision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reqs = append(a.reqs, r)
	return ApprovalDecision{Approved: true, Decided: true, ApproverID: "person:ana"}, nil
}

func subtaskExec(t *testing.T, h ToolHandler) (*ToolExecutor, *Subtasks) {
	t.Helper()
	reg := NewInMemoryToolRegistry()
	for _, st := range []SystemTool{
		{Name: "crm_search", Effects: []ToolEffect{EffectRead}},
		{Name: "web_search", Effects: []ToolEffect{EffectRead, EffectEgressRead}},
		{Name: "mail_send", Effects: []ToolEffect{EffectEgressSend}},
		{Name: "legacy_post", Effects: []ToolEffect{EffectEgress}},
		{Name: "memory_note", Effects: []ToolEffect{EffectWrite}},
		{Name: "schedule_task", Effects: []ToolEffect{EffectWrite}},
		{Name: "page_fetch", URLArgs: []string{"url"}}, // effects inferred from url_args
		{Name: "crm_update", Effects: []ToolEffect{EffectWrite}, Dangerous: true},
	} {
		reg.Register(st)
	}
	sub := NewSubtasks(0, nil)
	// OSS default: no authorizer at all (AllowAllAuthorizer) and the unrestricted
	// bypass — nothing but the subtask ceiling can stop a call.
	return &ToolExecutor{Registry: reg, Handler: h, InlineThreshold: 1 << 20, Unrestricted: true,
		Authz: AllowAllAuthorizer{}, Subtasks: sub, LeaseCeiling: sub}, sub
}

func bindChild(sub *Subtasks, lease LeaseID, id string, effects []ToolEffect, tools ...string) {
	sub.RegisterParent("parent-lease", "main")
	sub.Bind(lease, SubtaskBinding{SubtaskID: id, Label: "acme", ParentLease: "parent-lease", RootLease: "parent-lease",
		Depth: 1, AllowedEffects: effects, Tools: tools})
}

// The ceiling holds with AllowAllAuthorizer: read and egress_read run; write
// (memory notes, schedules), egress_send, plain egress and inferred egress are
// refused before dispatch. The same tools run on a non-subtask lease.
func TestSubtaskEffectCeiling_EnforcedByTheKernelUnderAllowAll(t *testing.T) {
	h := &countingHandler{result: []byte(`{"ok":true}`)}
	e, sub := subtaskExec(t, h)
	all := []string{"crm_search", "web_search", "mail_send", "legacy_post", "memory_note", "schedule_task", "page_fetch"}
	bindChild(sub, "child-1", "sub-1", []ToolEffect{EffectRead, EffectEgressRead}, all...)
	ctx := context.Background()
	for _, tool := range []string{"crm_search", "web_search"} {
		if got := e.Execute(ctx, ToolCallRequest{AgentID: "composed_worker", ToolName: tool, ArgsJSON: []byte(`{}`), SessionTokenID: "child-1"}); got.Denied || got.Error != "" {
			t.Fatalf("%s must run inside the ceiling: %+v", tool, got)
		}
	}
	for _, tool := range []string{"mail_send", "legacy_post", "memory_note", "schedule_task", "page_fetch"} {
		got := e.Execute(ctx, ToolCallRequest{AgentID: "composed_worker", ToolName: tool, ArgsJSON: []byte(`{"url":"https://x.test"}`), SessionTokenID: "child-1"})
		if !got.Denied || !strings.Contains(got.DenyReason, "subtask effect ceiling") {
			t.Fatalf("%s must be refused by the subtask ceiling: %+v", tool, got)
		}
		if tool == "page_fetch" && !strings.Contains(got.DenyReason, "inferred") {
			t.Fatalf("an inferred-egress refusal must say why: %s", got.DenyReason)
		}
	}
	if n := h.n.Load(); n != 2 {
		t.Fatalf("only the two in-ceiling calls may reach the handler, ran %d", n)
	}
	if _, web := sub.Report("child-1"); !web {
		t.Fatal("running web_search must mark the subtask as having read the web")
	}
	// A tool outside the child's toolset is refused by the lease ceiling.
	bindChild(sub, "child-2", "sub-2", []ToolEffect{EffectRead}, "crm_search")
	if got := e.Execute(ctx, ToolCallRequest{AgentID: "composed_worker", ToolName: "web_search", ArgsJSON: []byte(`{}`), SessionTokenID: "child-2"}); !got.Denied {
		t.Fatalf("a tool outside the subtask's toolset must be refused: %+v", got)
	}
	// The main agent's own lease is not bound: the same send runs (its approvals
	// and the person's access govern it, not the subtask ceiling).
	if got := e.Execute(ctx, ToolCallRequest{AgentID: "main", ToolName: "mail_send", ArgsJSON: []byte(`{}`), SessionTokenID: "parent-lease"}); got.Denied {
		t.Fatalf("the parent's lease is not under the subtask ceiling: %+v", got)
	}
}

// An approval raised inside a subtask carries the subtask's label to the person;
// a resumed subtask (a new child lease, the same deterministic subtask id) that
// re-issues the write is answered from the stored result — the write runs once.
func TestSubtaskApprovalLabelledAndResumeNeverRepeatsAWrite(t *testing.T) {
	h := &countingHandler{result: []byte(`{"id":"CRM-9"}`)}
	e, sub := subtaskExec(t, h)
	ap := &labelApproval{}
	e.Approval = ap
	widened := []ToolEffect{EffectRead, EffectWrite} // an operator-widened ceiling
	bindChild(sub, "child-a", "sub-same", widened, "crm_update")
	call := ToolCallRequest{AgentID: "composed_worker", ToolName: "crm_update", ArgsJSON: []byte(`{"id":"CRM-9","stage":"won"}`), SessionTokenID: "child-a"}
	first := e.Execute(context.Background(), call)
	if first.Denied || first.Error != "" {
		t.Fatalf("approved write must run: %+v", first)
	}
	if len(ap.reqs) != 1 || ap.reqs[0].Subtask != "acme" || !strings.Contains(ap.reqs[0].DuplicateNotice, `subtask "acme"`) {
		t.Fatalf("approval must be labelled with the subtask: %+v", ap.reqs)
	}
	sub.Forget("child-a")
	// Resume: the same subtask on a fresh lease, the arguments in another key order.
	bindChild(sub, "child-b", "sub-same", widened, "crm_update")
	call.SessionTokenID = "child-b"
	call.ArgsJSON = []byte(`{"stage":"won","id":"CRM-9"}`)
	again := e.Execute(context.Background(), call)
	if h.n.Load() != 1 || string(again.ResultJSON) != string(first.ResultJSON) {
		t.Fatalf("resumed subtask repeated the write: ran %d, again %+v", h.n.Load(), again)
	}
	if len(ap.reqs) != 1 {
		t.Fatalf("a write answered from its stored result must not ask again: %d approvals", len(ap.reqs))
	}
	// A second, deliberate identical write in the same subtask is a new occurrence.
	e.Execute(context.Background(), call)
	if h.n.Load() != 2 {
		t.Fatalf("the second occurrence of a call is a new call: ran %d", h.n.Load())
	}
	// A different subtask doing the same write is a different call.
	bindChild(sub, "child-c", "sub-other", widened, "crm_update")
	call.SessionTokenID = "child-c"
	e.Execute(context.Background(), call)
	if h.n.Load() != 3 {
		t.Fatalf("another subtask's write is its own: ran %d", h.n.Load())
	}
}

func TestSubtasksRegistry_PositionReserveAndOffer(t *testing.T) {
	sub := NewSubtasks(0, nil)
	if _, _, ok := sub.Position("planner-step-lease"); ok {
		t.Fatal("an unregistered lease (a planner-lane worker) is not a subtask parent")
	}
	sub.RegisterParent("p", "main")
	if d, root, ok := sub.Position("p"); !ok || d != 0 || root != "p" {
		t.Fatalf("parent position = %d %s %v", d, root, ok)
	}
	sub.Bind("c", SubtaskBinding{SubtaskID: "s", RootLease: "p", Depth: 1})
	if d, root, ok := sub.Position("c"); !ok || d != 1 || root != "p" {
		t.Fatalf("child position = %d %s %v", d, root, ok)
	}
	if err := sub.Reserve("p", 60, 64); err != nil {
		t.Fatal(err)
	}
	if err := sub.Reserve("p", 5, 64); err == nil {
		t.Fatal("past max_per_request must be refused")
	}
	if err := sub.Reserve("p", 4, 64); err != nil {
		t.Fatalf("a refused reservation must count nothing: %v", err)
	}
	if SubtaskIDFor("run", "p", []byte(`{"a":1,"b":2}`), []byte(`{"id":"x"}`)) != SubtaskIDFor("run", "p", []byte(`{"b":2,"a":1}`), []byte(`{"id":"x"}`)) {
		t.Fatal("subtask id must be deterministic over canonical arguments")
	}
	if SubtaskIDFor("run", "p", nil, []byte(`{"id":"x"}`)) == SubtaskIDFor("run", "p", nil, []byte(`{"id":"y"}`)) {
		t.Fatal("different briefs are different subtasks")
	}
}
