//go:build !windows

package sandbox

import (
	"slices"
	"strings"
	"testing"
)

// The usability probe makes every mount and namespace a kernel-only agent
// launch makes, so a host (a container engine) that allows only some of them
// is reported missing, never enforced-then-failing (ENG-2062).
func TestBwrapProbeCoversTheAgentLaunchFlags(t *testing.T) {
	p := DefaultPolicy()
	tp := TierPolicy{ConfineFS: true, HideState: true, DenyNetwork: true}
	spec := Spec{ServerID: "a", Tier: TierThirdParty, Agent: &AgentSpec{}}
	launch := bwrapArgs("bwrap", p, spec, tp, "/w", "", true)
	probe := bwrapProbeArgs(true)
	// Flags that need a namespace or a fresh mount; binds of real paths are
	// launch-specific and need no extra privilege once a mount works.
	for _, f := range []string{"--dev", "--proc", "--tmpfs", "--unshare-pid", "--unshare-ipc",
		"--unshare-uts", "--unshare-net", "--unshare-user", "--new-session", "--die-with-parent"} {
		if slices.Contains(launch, f) && !slices.Contains(probe, f) {
			t.Errorf("the launch uses %s but the probe does not: %s", f, strings.Join(probe, " "))
		}
	}
	if slices.Contains(bwrapProbeArgs(false), "--unshare-user") {
		t.Error("a root kernel's probe must not ask for a user namespace")
	}
	if probe[len(probe)-1] != "/bin/true" || probe[len(probe)-2] != "--" {
		t.Errorf("probe must end with -- /bin/true: %v", probe)
	}
}
