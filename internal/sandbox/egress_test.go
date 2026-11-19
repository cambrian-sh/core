package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/internal/sandbox/egressproxy"
)

// startedBroker is a running egress proxy, stopped at cleanup.
func startedBroker(t *testing.T) *egressproxy.Broker {
	t.Helper()
	b := egressproxy.New(egressproxy.Options{})
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Stop)
	return b
}

func TestEgressPlanDecidesWhatIsEnforced(t *testing.T) {
	p := DefaultPolicy()
	p.Egress = startedBroker(t)
	third := func(hosts ...string) Spec {
		return p.Resolve("s", "third-party", "", false, ServerSpec{Network: NetworkEgress, Hosts: hosts})
	}
	cases := []struct {
		name string
		spec Spec
		want bool
		why  string
	}{
		{"third-party with hosts", third("api.example.com", "*.slack.com"), true, ""},
		{"no host list", third(), false, ""},
		{"any host", third("pypi.org", "*"), false, `"*" declares any host`},
		{"placeholder", third("registry.npmjs.org", "the configured database host"), false, "is not a host name"},
		{"vetted keeps the network", p.Resolve("s", "vetted", "", false, ServerSpec{Network: NetworkEgress, Hosts: []string{"api.example.com"}}), false, "keeps the network"},
		{"no egress declared", p.Resolve("s", "third-party", "", false, ServerSpec{}), false, ""},
	}
	for _, c := range cases {
		got, why := egressPlan(p, c.spec, c.spec.tierPolicy(p))
		if got != c.want || !strings.Contains(why, c.why) {
			t.Errorf("%s: egressPlan = %v %q, want %v containing %q", c.name, got, why, c.want, c.why)
		}
	}
	// No proxy in the kernel: recorded, not filtered, and said so.
	bare := DefaultPolicy()
	s := bare.Resolve("s", "third-party", "", false, ServerSpec{Network: NetworkEgress, Hosts: []string{"api.example.com"}})
	if ok, why := egressPlan(bare, s, s.tierPolicy(bare)); ok || !strings.Contains(why, "no egress proxy") {
		t.Fatalf("no broker: %v %q", ok, why)
	}
}

func TestPlanReportsEnforcedOrRecordedEgress(t *testing.T) {
	p := DefaultPolicy()
	spec := p.Resolve("s", "third-party", "", false, ServerSpec{Network: NetworkEgress, Hosts: []string{"api.example.com"}})
	tp := spec.tierPolicy(p)
	base := map[Control]string{ControlEnv: "", ControlWorkdir: "", ControlKillTree: "docker", ControlOutputCap: "",
		ControlStallKill: "", ControlMemory: "docker", ControlProcesses: "docker", ControlCPUTime: "docker",
		ControlFSConfine: "docker", ControlNetwork: "docker", ControlLowPrivilege: "docker"}
	enforced := plan(spec, tp, capability{runner: RunnerDocker, enforced: base, egress: RunnerDocker})
	notes := strings.Join(enforced.Notes, ";")
	if !slices.Contains(enforced.Enforced, ControlNetwork) || !slices.Contains(enforced.Mechanisms, MechanismEgressProxy) ||
		!strings.Contains(notes, "api.example.com (enforced") || strings.Contains(notes, "not filtered") {
		t.Fatalf("enforced egress report: %+v", enforced)
	}
	recorded := plan(spec, tp, capability{runner: RunnerNative, enforced: base, egressWhy: "no egress proxy plumbing on this runner"})
	notes = strings.Join(recorded.Notes, ";")
	if slices.Contains(recorded.Enforced, ControlNetwork) || slices.Contains(recorded.Mechanisms, MechanismEgressProxy) ||
		!strings.Contains(notes, "hosts recorded, not filtered: no egress proxy plumbing on this runner") {
		t.Fatalf("recorded egress report: %+v", recorded)
	}
}

// egressProbe is a contained "server" that checks, from inside, what its
// network reaches and prints one JSON line: an allowed host through the proxy,
// an undeclared host through the proxy, an IP literal through the proxy, and a
// direct connection that bypasses the proxy.
const egressProbe = `
import json, os, socket, sys, urllib.request
def get(url):
    try:
        return str(urllib.request.urlopen(url, timeout=20).status)
    except Exception as e:
        return "error: " + str(e)
def direct(host, port):
    try:
        socket.create_connection((host, port), timeout=5).close()
        return "connected"
    except Exception as e:
        return "error: " + str(e)
print(json.dumps({"proxy": os.environ.get("HTTPS_PROXY", ""),
    "allowed": get("https://example.com/"),
    "undeclared": get("https://www.wikipedia.org/"),
    "literal": get("https://1.1.1.1/"),
    "direct": direct("1.1.1.1", 443)}), flush=True)
sys.stdin.read()
`

type probeResult struct {
	Proxy, Allowed, Undeclared, Literal, Direct string
}

func readProbe(t *testing.T, proc *Process) probeResult {
	t.Helper()
	lines := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(proc)
		if sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	select {
	case line, ok := <-lines:
		if !ok {
			t.Fatal("the probe printed nothing")
		}
		var r probeResult
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("probe output %q: %v", line, err)
		}
		return r
	case <-time.After(4 * time.Minute):
		t.Fatal("the probe did not answer")
	}
	return probeResult{}
}

// assertEnforced checks the probe saw exactly the declared host.
func assertEnforced(t *testing.T, r probeResult, rep Report) {
	t.Helper()
	t.Logf("probe: %+v", r)
	t.Logf("containment: %s", rep.Summary())
	if r.Proxy == "" {
		t.Error("the server was not told about the proxy")
	}
	if r.Allowed != "200" {
		t.Errorf("declared host example.com: %q, want 200 through the proxy", r.Allowed)
	}
	if !strings.Contains(r.Undeclared, "403") {
		t.Errorf("undeclared host: %q, want a 403 refusal", r.Undeclared)
	}
	if !strings.Contains(r.Literal, "403") {
		t.Errorf("IP literal: %q, want a 403 refusal", r.Literal)
	}
	if !strings.HasPrefix(r.Direct, "error") {
		t.Errorf("a direct connection bypassing the proxy: %q, want no route", r.Direct)
	}
	if !slices.Contains(rep.Enforced, ControlNetwork) || !slices.Contains(rep.Mechanisms, MechanismEgressProxy) ||
		!strings.Contains(strings.Join(rep.Notes, ";"), "(enforced") {
		t.Errorf("report does not say enforced: %s", rep.Summary())
	}
}

// TestLiveDockerEgressAllowlistEnforced (ENG-688) launches a third-party
// server under the docker runner with a declared host list and checks from
// inside the container that only the declared host is reachable, through the
// kernel's proxy. It needs a Linux docker engine and the internet:
//
//	SANDBOX_EGRESS_E2E=1 go test -run TestLiveDockerEgress ./internal/sandbox/
//
// SANDBOX_EGRESS_IMAGE overrides the Python image (forwarder and probe).
func TestLiveDockerEgressAllowlistEnforced(t *testing.T) {
	if os.Getenv("SANDBOX_EGRESS_E2E") != "1" {
		t.Skip("live check: set SANDBOX_EGRESS_E2E=1 (needs docker and the internet)")
	}
	if _, why := dockerReachable(context.Background(), ""); why != "" {
		t.Fatalf("docker: %s", why)
	}
	image := os.Getenv("SANDBOX_EGRESS_IMAGE")
	if image == "" {
		image = "python:3.13-slim"
	}
	if out, err := exec.Command("docker", "image", "inspect", image).CombinedOutput(); err != nil { //nolint:gosec // test
		if out, err := exec.Command("docker", "pull", image).CombinedOutput(); err != nil { //nolint:gosec // test
			t.Fatalf("pull %s: %v %s", image, err, out)
		}
		_ = out
	}
	p := testPolicy(t)
	p.Egress = startedBroker(t)
	p.DockerImages["python3"] = image
	spec := p.Resolve("egress-probe", "third-party", "", false, ServerSpec{
		Network: NetworkEgress, Hosts: []string{"example.com"}, Runner: RunnerDocker, Image: image})
	proc, rep, err := Start(context.Background(), spec, Command{Path: "python3", Args: []string{"-I", "-c", egressProbe}})
	if err != nil {
		t.Fatalf("start: %v (%s)", err, rep.Summary())
	}
	r := readProbe(t, proc)
	if err := proc.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	assertEnforced(t, r, rep)
	// The sidecar goes with the server.
	out, _ := exec.Command("docker", "ps", "-aq", "--filter", "label=cambrian.sandbox=mcp-egress", "--filter", "label=cambrian.server=egress-probe").Output() //nolint:gosec // test
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("egress sidecar left behind: %s", out)
	}
}
