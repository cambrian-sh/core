//go:build linux

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// TestBwrapArgsEgressNamespaceAndForwarder: an enforced egress allowlist
// gets its own network namespace and the forwarder bound into the private
// /tmp; without a proxy the declared egress keeps the host network, as
// before ENG-688.
func TestBwrapArgsEgressNamespaceAndForwarder(t *testing.T) {
	p := DefaultPolicy()
	p.Egress = startedBroker(t)
	spec := p.Resolve("s", "third-party", "", false, ServerSpec{Network: NetworkEgress, Hosts: []string{"example.com"}})
	a := bwrapArgs("bwrap", p, spec, spec.tierPolicy(p), "/w", "", true)
	if !slices.Contains(a, "--unshare-net") || !slices.Contains(a, egressForwarderPath) {
		t.Fatalf("enforced egress argv: %v", a)
	}
	p.Egress = nil
	a = bwrapArgs("bwrap", p, spec, spec.tierPolicy(p), "/w", "", true)
	if slices.Contains(a, "--unshare-net") || slices.Contains(a, egressForwarderPath) {
		t.Fatalf("unenforced egress must keep the network: %v", a)
	}
}

// TestLiveBwrapEgressAllowlistEnforced (ENG-688) is the bubblewrap runner's
// live check: a third-party server declaring hosts=[example.com] runs under
// bwrap in its own network namespace with the kernel binary (here: the test
// binary) as the forwarder, and from inside reaches only example.com.
//
//	SANDBOX_EGRESS_E2E=1 go test -run TestLiveBwrapEgress ./internal/sandbox/
//
// Needs usable bubblewrap, python3 and the internet.
func TestLiveBwrapEgressAllowlistEnforced(t *testing.T) {
	if os.Getenv("SANDBOX_EGRESS_E2E") != "1" {
		t.Skip("live check: set SANDBOX_EGRESS_E2E=1 (needs bubblewrap, python3 and the internet)")
	}
	if _, why := bwrapUsable(""); why != "" {
		t.Fatalf("bubblewrap: %s", why)
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("python3 not found")
	}
	p := testPolicy(t)
	p.Egress = startedBroker(t)
	// A non-root kernel has no lower uid to drop to (low_privilege is the
	// root-kernel CI step's business); everything else is the shipped tier.
	tp := p.Tiers[TierThirdParty]
	tp.LowPrivilege = os.Geteuid() == 0
	p.Tiers[TierThirdParty] = tp
	spec := p.Resolve("egress-probe", "third-party", "", false, ServerSpec{
		Network: NetworkEgress, Hosts: []string{"example.com"}})
	proc, rep, err := Start(context.Background(), spec, Command{Path: py, Args: []string{"-I", "-c", egressProbe},
		Env: []string{"PATH=/usr/local/bin:/usr/bin:/bin"}})
	if err != nil {
		t.Fatalf("start: %v (%s)", err, rep.Summary())
	}
	r := readProbe(t, proc)
	if err := proc.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	if !slices.Contains(rep.Mechanisms, "bwrap") {
		t.Errorf("not launched under bubblewrap: %s", rep.Summary())
	}
	assertEnforced(t, r, rep)
	if !strings.Contains(r.Proxy, "127.0.0.1:3128") {
		t.Errorf("proxy env: %q", r.Proxy)
	}
}
