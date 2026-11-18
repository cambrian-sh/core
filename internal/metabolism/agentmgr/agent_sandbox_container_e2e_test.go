package agentmgr

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/internal/sandbox"
)

// ENG-2062: the containment report of the kernel image, with and without the
// container engine settings bubblewrap needs (docs/container-sandbox.md). Run
// inside the image as its runtime user with
//
//	AGENTMGR_E2E_EXPECT_CONTAINMENT=enforced  (container started with the settings)
//	AGENTMGR_E2E_EXPECT_CONTAINMENT=missing   (container started without them)
//
// enforced: a first-party agent gets fs_confine and hide_state from bwrap.
// missing: the report names both missing with the reason, a first-party agent
// is degraded (not refused) and nothing claims enforcement.
// Both: a third-party agent is refused — under the default refuse policy it
// needs low_privilege, which a non-root kernel cannot give (and, without the
// settings, fs_confine too), exactly as on a bare-metal host.
func TestE2E_ContainerContainmentReport(t *testing.T) {
	want := os.Getenv("AGENTMGR_E2E_EXPECT_CONTAINMENT")
	if want == "" {
		t.Skip("set AGENTMGR_E2E_EXPECT_CONTAINMENT=enforced|missing inside the kernel image")
	}
	sb := NewAgentSandbox(AgentSandboxOptions{WorkRoot: e2eRoot(t), ThirdPartyRefuse: true})
	plan := func(tier sandbox.Tier) sandbox.Report {
		return sandbox.Plan(context.Background(), sandbox.Spec{ServerID: "plan", Tier: tier,
			Policy: sb.Policy(), Agent: &sandbox.AgentSpec{}}, sandbox.Command{Path: "x"})
	}
	first, third := plan(sandbox.TierFirstParty), plan(sandbox.TierThirdParty)
	t.Logf("first-party: %s", first.Summary())
	t.Logf("third-party: %s", third.Summary())
	missing := func(r sandbox.Report, c sandbox.Control) string {
		for _, m := range r.Missing {
			if strings.HasPrefix(m, string(c)+":") {
				return m
			}
		}
		return ""
	}
	switch want {
	case "enforced":
		for _, c := range []sandbox.Control{sandbox.ControlFSConfine, sandbox.ControlHideState} {
			if !slices.Contains(first.Enforced, c) {
				t.Errorf("first-party: %s not enforced: %s", c, first.Summary())
			}
		}
		if first.Refused {
			t.Errorf("first-party refused: %s", first.Summary())
		}
	case "missing":
		for _, c := range []sandbox.Control{sandbox.ControlFSConfine, sandbox.ControlHideState} {
			if slices.Contains(first.Enforced, c) {
				t.Errorf("first-party claims %s enforced without the engine settings: %s", c, first.Summary())
			}
			if m := missing(first, c); !strings.Contains(m, "bubblewrap") {
				t.Errorf("first-party: %s missing reason does not name bubblewrap: %q", c, m)
			}
		}
		if !first.Degraded() {
			t.Errorf("first-party must be degraded (default degrade), got: %s", first.Summary())
		}
	default:
		t.Fatalf("AGENTMGR_E2E_EXPECT_CONTAINMENT=%q: want enforced or missing", want)
	}
	if !third.Refused {
		t.Errorf("third-party must be refused here: %s", third.Summary())
	}
	if os.Geteuid() != 0 && missing(third, sandbox.ControlLowPrivilege) == "" {
		t.Errorf("third-party under a non-root kernel must name low_privilege missing: %s", third.Summary())
	}
}
