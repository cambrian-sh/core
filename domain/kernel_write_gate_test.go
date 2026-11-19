package domain

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// ENG-2247 (owner decision 2026-11-19: gate every write call too): the dedicated
// kernel write calls are decided by the SAME gate object and raise the approval
// through the SAME controller a gated tool call does.

func kernelWriteGate(t *testing.T, verbs map[string][]ToolEffect) *RuleOfTwoGate {
	t.Helper()
	g, err := NewRuleOfTwoGate(RuleOfTwoPolicy{
		UntrustedSources:   []SourceClass{SourceWeb, SourceTool, SourceOtherAgent, SourceIngested},
		GatedEffects:       []ToolEffect{EffectWrite, EffectEgressSend, EffectSpend, EffectAdmin},
		KernelWriteEffects: verbs,
	}, NewProvenanceTracker(0, nil))
	if err != nil {
		t.Fatal(err)
	}
	return g
}

var noteCall = KernelWriteCall{Verb: "propose_memory_note", AgentID: "a", ArgsPreview: "append personal notes", Args: []byte(`{"content":"x"}`)}

func TestRuleOfTwoGate_KernelWriteEffectsAreValidated(t *testing.T) {
	for name, verbs := range map[string]map[string][]ToolEffect{
		"unknown effect": {"propose_memory_note": {"delete"}},
		"no effect":      {"propose_memory_note": nil},
		"empty verb":     {" ": {EffectWrite}},
	} {
		_, err := NewRuleOfTwoGate(RuleOfTwoPolicy{
			UntrustedSources: []SourceClass{SourceWeb}, GatedEffects: []ToolEffect{EffectWrite},
			KernelWriteEffects: verbs,
		}, NewProvenanceTracker(0, nil))
		if !errors.Is(err, ErrRuleOfTwoPolicy) {
			t.Errorf("%s: want a policy error, got %v", name, err)
		}
	}
}

func TestRuleOfTwoGate_KernelWriteToolCarriesTheConfiguredEffects(t *testing.T) {
	g := kernelWriteGate(t, map[string][]ToolEffect{"propose_memory_note": {EffectWrite}, "lookup": {EffectRead}})
	scope := NewTaintScope("L", LeaseBinding{ConversationID: "c1"}, "", "")
	g.Observe(scope, SourceIngested, nil)
	if hit, gated := g.Check(scope, g.KernelWriteTool("propose_memory_note"), nil); !gated || len(hit.Effects) != 1 || hit.Effects[0] != EffectWrite {
		t.Fatalf("a declared write verb must be gated on write: %+v %v", hit, gated)
	}
	if _, gated := g.Check(scope, g.KernelWriteTool("lookup"), nil); gated {
		t.Fatal("a verb declared read is not a write")
	}
	// An undeclared verb carries no effects: gated on everything (fail closed).
	if hit, gated := g.Check(scope, g.KernelWriteTool("brand_new_verb"), nil); !gated || len(hit.Effects) == 0 {
		t.Fatalf("an undeclared verb must be gated: %+v %v", hit, gated)
	}
	var nilGate *RuleOfTwoGate
	if tool := nilGate.KernelWriteTool("x"); tool.Name != "x" || len(tool.Effects) != 0 {
		t.Fatalf("nil gate: %+v", tool)
	}
}

func TestGateKernelWrite_TaintedSessionAsksWithTheNoticeControlDoesNot(t *testing.T) {
	appr := &recordingApproval{approve: true}
	g := kernelWriteGate(t, map[string][]ToolEffect{"propose_memory_note": {EffectWrite}})
	e := ruleOfTwoExec(t, appr, &fakeHandler{}, g)

	// The control session read nothing untrusted: the note is written unasked.
	control := WithTaintScope(context.Background(), NewTaintScope("L0", LeaseBinding{ConversationID: "c0"}, "", ""))
	if v := e.GateKernelWrite(control, noteCall); !v.Allowed || v.Gated || len(appr.reqs) != 0 {
		t.Fatalf("control session: %+v approvals=%d", v, len(appr.reqs))
	}
	if notice, gated := e.RuleOfTwoKernelWriteNotice(control, noteCall); gated || notice != "" {
		t.Fatalf("control session notice: %q", notice)
	}

	// The tainted session recalled an ingested document (QueryMemory's ObserveContent).
	scope := NewTaintScope("L1", LeaseBinding{ConversationID: "c1"}, "", "c1")
	tainted := WithConversationOwner(WithTaintScope(context.Background(), scope), "user:emp:1")
	e.ObserveContent(tainted, SourceIngested, nil)
	v := e.GateKernelWrite(tainted, noteCall)
	if !v.Gated || !v.Allowed || v.ApproverID != "op" {
		t.Fatalf("tainted session, approved: %+v", v)
	}
	if len(appr.reqs) != 1 {
		t.Fatalf("want one approval, got %d", len(appr.reqs))
	}
	r := appr.reqs[0]
	if !strings.HasPrefix(r.DuplicateNotice, "Rule of Two:") || !strings.Contains(r.DuplicateNotice, "ingested") ||
		r.ToolName != "propose_memory_note" || r.AgentID != "a" || r.Beneficiary != PersonPrincipal("user:emp:1") {
		t.Fatalf("approval request: %+v", r)
	}
	if notice, gated := e.RuleOfTwoKernelWriteNotice(tainted, noteCall); !gated || !strings.HasPrefix(notice, "Rule of Two:") {
		t.Fatalf("tainted notice: %q %v", notice, gated)
	}

	// Declined: not allowed, with the reason.
	appr.approve = false
	if v := e.GateKernelWrite(tainted, noteCall); v.Allowed || !v.Gated || v.DenyReason == "" || v.ApprovalState != ApprovalDenied {
		t.Fatalf("declined: %+v", v)
	}
}

func TestGateKernelWrite_FailsClosedWithoutApprovalAndIsInertWhenOff(t *testing.T) {
	g := kernelWriteGate(t, map[string][]ToolEffect{"propose_memory_note": {EffectWrite}})
	e := ruleOfTwoExec(t, nil, &fakeHandler{}, g)
	scope := NewTaintScope("L", LeaseBinding{ConversationID: "c"}, "", "")
	g.Observe(scope, SourceWeb, nil)
	v := e.GateKernelWrite(WithTaintScope(context.Background(), scope), noteCall)
	if v.Allowed || !v.Gated || !strings.Contains(v.DenyReason, "Rule of Two") {
		t.Fatalf("no approval controller must deny a gated write: %+v", v)
	}
	// A zero scope (the kernel cannot tell whose session) is tainted (fail closed).
	if v := e.GateKernelWrite(context.Background(), noteCall); v.Allowed || !v.Gated {
		t.Fatalf("unknown provenance must be gated: %+v", v)
	}
	// Gate off: every kernel write proceeds unasked.
	off := ruleOfTwoExec(t, nil, &fakeHandler{}, nil)
	if v := off.GateKernelWrite(context.Background(), noteCall); !v.Allowed || v.Gated {
		t.Fatalf("gate off: %+v", v)
	}
	var nilExec *ToolExecutor
	if v := nilExec.GateKernelWrite(context.Background(), noteCall); !v.Allowed {
		t.Fatalf("nil executor: %+v", v)
	}
}
