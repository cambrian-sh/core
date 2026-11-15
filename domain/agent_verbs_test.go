package domain

import (
	"context"
	"testing"
)

// AgentVerbAllowed (owner option A, T-20261113-agent-proposal-rpcs-proposeknowledge):
// the tool-grant check under the verb's name, plus a profile `tools:` entry.

type leaseCeilingStub map[string][]string // lease → allowed tools

func (l leaseCeilingStub) AllowsLease(lease, tool string) (bool, bool) {
	allow, ok := l[lease]
	if !ok {
		return false, false
	}
	for _, t := range allow {
		if t == tool {
			return true, true
		}
	}
	return false, true
}

func TestAgentVerbAllowed(t *testing.T) {
	ctx := context.Background()
	const v = VerbProposeKnowledge
	granted := NewInMemoryGrantsStore()
	granted.Set("granted", []ToolGrant{{Tool: v}})
	granted.Set("other-verb", []ToolGrant{{Tool: VerbProposeOntologyChange}})
	ceiling := NewProfileToolCeiling()
	ceiling.Set("profile-with", ToolAllowlist{"web_*", "propose_*"})
	ceiling.Set("profile-without", ToolAllowlist{"web_*"})
	ceiling.Set("granted-but-capped", ToolAllowlist{"web_*"})
	granted.Set("granted-but-capped", []ToolGrant{{Tool: v}})

	restricted := &ToolExecutor{Grants: granted, ceiling: ceiling}
	unrestricted := &ToolExecutor{unrestricted: true, Grants: NewInMemoryGrantsStore(), ceiling: ceiling}
	leased := &ToolExecutor{Grants: granted, ceiling: ceiling, leaseCeiling: leaseCeilingStub{"child": {"web_fetch"}}}

	for _, c := range []struct {
		name  string
		e     *ToolExecutor
		agent string
		lease string
		want  bool
	}{
		{"restricted, no grant", restricted, "plain", "", false},
		{"restricted, grant of the verb", restricted, "granted", "", true},
		{"restricted, grant of another verb only", restricted, "other-verb", "", false},
		{"profile tools: names the verb", restricted, "profile-with", "", true},
		{"profile tools: does not name it", restricted, "profile-without", "", false},
		{"a grant outside the profile ceiling", restricted, "granted-but-capped", "", false},
		{"unrestricted", unrestricted, "plain", "", true},
		{"unrestricted, but the profile ceiling excludes it", unrestricted, "profile-without", "", false},
		{"granted, but the lease ceiling excludes it", leased, "granted", "child", false},
		{"granted, a lease with no ceiling", leased, "granted", "top", true},
		{"profile names it, but the lease ceiling excludes it", leased, "profile-with", "child", false},
		{"profile names it, a lease with no ceiling", leased, "profile-with", "top", true},
		{"anonymous", unrestricted, "", "", false},
		{"no grants store in restricted mode", &ToolExecutor{}, "granted", "", false},
		{"nil executor", nil, "granted", "", false},
	} {
		if got := c.e.AgentVerbAllowed(ctx, c.agent, v, c.lease); got != c.want {
			t.Errorf("%s: allowed=%v, want %v", c.name, got, c.want)
		}
	}
}
