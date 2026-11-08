package domain

import (
	"context"
	"strings"
	"testing"
)

// T-20261107-acting-for-user-is-denied-their-own-finance: a TAG denial used to be
// rendered "effect not permitted: <clause>", which sent the reader to the effect
// policy. Each check now names itself.
func TestToolAccessDenyReason_NamesTheCheck(t *testing.T) {
	ctx := WithConversationOwner(context.Background(), "praman")
	tag := ToolAccessDenyReason(ctx, AccessDecision{Reason: ReasonAnyOfUnsatisfied, Detail: "finance|shared:x"}, nil)
	for _, want := range []string{"classification not permitted", "anyof_unsatisfied", "finance|shared:x", "none (unclassified tool)", "acting for user:praman"} {
		if !strings.Contains(tag, want) {
			t.Errorf("tag denial %q lacks %q", tag, want)
		}
	}
	if strings.Contains(tag, "effect not permitted") {
		t.Errorf("tag denial labelled as an effect denial: %q", tag)
	}
	eff := ToolAccessDenyReason(ctx, AccessDecision{Reason: ReasonEffectNotPermitted, Detail: "egress"}, []string{"finance"})
	if eff != "effect not permitted: egress" {
		t.Errorf("effect denial = %q", eff)
	}
	who := ToolAccessDenyReason(context.Background(), AccessDecision{Reason: ReasonNoPrincipal, Detail: "unknown principal agent:x"}, nil)
	if who != "principal not authorised: unknown principal agent:x" {
		t.Errorf("principal denial = %q", who)
	}
	forb := ToolAccessDenyReason(context.Background(), AccessDecision{Reason: ReasonForbiddenTag, Detail: "hr"}, []string{"hr", "crm"})
	if !strings.Contains(forb, "forbidden_tag") || !strings.Contains(forb, "hr,crm") || strings.Contains(forb, "acting for") {
		t.Errorf("forbidden denial = %q", forb)
	}
}
