package app

import (
	"testing"

	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
	"github.com/cambrian-sh/core/internal/substrate/operator"
	"github.com/cambrian-sh/core/internal/substrate/session"
)

// T-20261118-operator-handshake-never-advertises-authored: the "authored-plans"
// capability is derived from Options.HasPlanSubmitter, so the submitter must be
// decided from the same kernel conditions that make SubmitPlan servable, and an
// absent submitter must be an untyped nil — a typed-nil planSubmitter would make
// HasPlanSubmitter report true on a kernel that cannot run a plan.
func TestPlanSubmitterFor_WiredExactlyWhenSessionsAndServerExist(t *testing.T) {
	known := func(string) bool { return true }
	cases := []struct {
		name string
		k    *Kernel
		want bool
	}{
		{"no session manager, no server", &Kernel{}, false},
		{"session manager only", &Kernel{SessionMgr: session.New(nil)}, false},
		{"server only", &Kernel{Server: &subnetwork.Server{}}, false},
		{"session manager and server", &Kernel{SessionMgr: session.New(nil), Server: &subnetwork.Server{}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ps := planSubmitterFor(tc.k, known)
			if got := ps != nil; got != tc.want {
				t.Fatalf("planSubmitterFor wired = %v, want %v", got, tc.want)
			}
			if got := (operator.Options{PlanSubmitter: ps}).HasPlanSubmitter(); got != tc.want {
				t.Fatalf("Options.HasPlanSubmitter = %v, want %v (it gates \"authored-plans\")", got, tc.want)
			}
		})
	}
}
