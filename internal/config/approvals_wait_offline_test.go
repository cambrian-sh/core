package config

import (
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ENG-653: approvals.rules[].wait_offline reaches the policy row (a validated
// key must be read, AGENTS.md §3.4); absent means false.
func TestApprovalsConfig_WaitOfflineReachesThePolicy(t *testing.T) {
	c := ApprovalsConfig{Rules: []ApprovalRuleConfig{
		{Class: domain.ApprovalKindMachineConsent, Approvers: []string{"self"}, WaitOffline: true},
		{Class: "payment", Approvers: []string{"operator"}},
	}}
	got := map[string]bool{}
	for _, r := range c.PolicyConfig().Rules {
		got[r.Class] = r.WaitOffline
	}
	if !got[domain.ApprovalKindMachineConsent] || got["payment"] {
		t.Fatalf("wait_offline per class = %v", got)
	}
}
