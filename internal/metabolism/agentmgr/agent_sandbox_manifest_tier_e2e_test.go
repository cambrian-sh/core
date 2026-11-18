package agentmgr

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/mapper"
	"github.com/cambrian-sh/core/internal/sandbox"
	"github.com/cambrian-sh/core/internal/storage"
)

// probeDials dials every host:port in AGENTMGR_PROBE_DIAL (comma-separated)
// from inside the probe agent and records what it reached.
func probeDials(rep *probeReport) {
	targets := os.Getenv("AGENTMGR_PROBE_DIAL")
	if targets == "" {
		return
	}
	rep.Dial = map[string]string{}
	for _, addr := range strings.Split(targets, ",") {
		c, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			rep.Dial[addr] = "denied: " + err.Error()
			continue
		}
		_ = c.Close()
		rep.Dial[addr] = "ok"
	}
}

// localTargets are TCP listeners this test owns — a loopback one and, when
// the host has one, one on a non-loopback interface address: hosts that are
// not the kernel's agent plane. Each accepts and drops connections.
func localTargets(t *testing.T) []string {
	t.Helper()
	hosts := []string{"127.0.0.1"}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
				hosts = append(hosts, ipn.IP.String())
				break
			}
		}
	}
	var out []string
	for _, h := range hosts {
		l, err := net.Listen("tcp", net.JoinHostPort(h, "0"))
		if err != nil {
			continue
		}
		t.Cleanup(func() { _ = l.Close() })
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				_ = c.Close()
			}
		}()
		out = append(out, l.Addr().String())
	}
	return out
}

// manifestAgentFromDisk writes agents/<id>.py with an AGENT_MANIFEST naming
// tier ("" ⇒ no trust_tier key) and returns the definition the REAL chain
// yields: storage.DiscoverFilesystemAgentsWith → UpsertDiscoveredAgent → the
// registry record → mapper.ToDomain (exactly what the kernel launches from).
func manifestAgentFromDisk(t *testing.T, root, id, tier string) *domain.AgentDefinition {
	t.Helper()
	agents := filepath.Join(root, "agents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	m := `{"version": "1.0.0"}`
	if tier != "" {
		m = `{"version": "1.0.0", "trust_tier": "` + tier + `"}`
	}
	src := "AGENT_DESCRIPTION = \"network probe\"\n\nAGENT_MANIFEST = '''\n" + m + "\n'''\n"
	if err := os.WriteFile(filepath.Join(agents, id+".py"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	found, err := storage.DiscoverFilesystemAgentsWith(agents, domain.IsSystemAgent, storage.DiscoverOptions{})
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewBBoltAdapterNoScan(filepath.Join(root, id+".db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, da := range found {
		if err := store.UpsertDiscoveredAgent(da); err != nil {
			t.Fatal(err)
		}
	}
	rec, err := store.GetAgentRecord(id)
	if err != nil || rec == nil {
		t.Fatalf("agent %s not registered: %v", id, err)
	}
	def := mapper.NewAgentMapper().ToDomain(*rec)
	return &def
}

// bootProbe launches def (its exec swapped for the probe, nothing else) under
// the default execution.agents.sandbox and returns the probe's report, or the
// boot error.
func bootProbe(t *testing.T, root string, def *domain.AgentDefinition, dial []string) (*probeReport, error) {
	t.Helper()
	sbc := config.DefaultConfig().Execution.Agents.Sandbox
	if !sbc.Enabled {
		t.Fatal("execution.agents.sandbox is on by default since ADR-0157 S7 (core 2615539)")
	}
	sb := NewAgentSandbox(AgentSandboxOptions{
		WorkRoot: filepath.Join(root, "w"), KernelDir: filepath.Join(root, "kernel"),
		KernelStatePaths: []string{filepath.Join(root, "data")},
		FirstPartyRefuse: sbc.FirstParty.Refuses(), ThirdPartyRefuse: sbc.ThirdParty.Refuses(),
		KernelOnlyAll: sbc.KernelOnlyAll(),
	})
	im := NewInstanceManager("127.0.0.1:1")
	im.SetAgentContainment(AgentContainment{Sandbox: sb})
	im.SetAgentTransport(AgentTransport{KernelSocket: listenKernelSocketForTest})
	im.SetEnvExtras([]string{"AGENTMGR_CHAOS_HELPER=probe", "AGENTMGR_PROBE_DIAL=" + strings.Join(dial, ",")})
	exe := probeExe(t, root)
	def.Runtime, def.ExecPath, def.Dir = domain.RuntimeBinary, exe, filepath.Dir(exe)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	inst, err := im.GetOrBootInstance(ctx, def, "")
	if err != nil {
		return nil, err
	}
	defer func() {
		im.mu.Lock()
		pe := im.exits[inst.ID]
		im.mu.Unlock()
		im.EvictInstance(inst.ID)
		if pe != nil {
			select {
			case <-pe.done:
			case <-time.After(30 * time.Second):
				t.Error("the evicted agent did not exit")
			}
		}
	}()
	if err := waitHealthy(ctx, inst.SocketPath, 15*time.Second); err != nil {
		t.Fatalf("the kernel cannot reach the agent: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(inst.SocketPath), "probe.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s probe: %s", def.ID, raw)
	var rep probeReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	return &rep, nil
}

// ENG-2085 end to end: a local agent whose AGENT_MANIFEST declares
// trust_tier third-party is discovered at third-party and launched at it
// under the default sandbox — kernel-only network: it cannot reach a host that
// is not the kernel's agent plane — where the same agent without the
// declaration runs first-party. Where this host cannot give third-party its
// controls (the default refuse policy), the boot must be refused — never run
// first-party.
func TestE2E_ManifestThirdPartyAgentBootsKernelOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("launches real subprocesses; skipped in -short")
	}
	root := e2eRoot(t)
	third := manifestAgentFromDisk(t, root, "netprobe_third_agent", "third-party")
	if third.TrustTier != "third-party" {
		t.Fatalf("the manifest's trust_tier did not reach the definition: %+v", third)
	}
	if got := agentTrustTier(third); got != agentTierThirdParty {
		t.Fatalf("agentTrustTier = %q, want third-party", got)
	}
	targets := localTargets(t)
	if os.Getenv("AGENTMGR_E2E_OFFLINE") == "" {
		targets = append(targets, "1.1.1.1:443") // the S7 finding's host
	}

	sbc := config.DefaultConfig().Execution.Agents.Sandbox
	plan := sandbox.Plan(context.Background(), sandbox.Spec{ServerID: "plan", Tier: sandbox.TierThirdParty,
		Policy: NewAgentSandbox(AgentSandboxOptions{WorkRoot: filepath.Join(root, "w"), ThirdPartyRefuse: sbc.ThirdParty.Refuses()}).Policy(),
		Agent:  &sandbox.AgentSpec{}}, sandbox.Command{Path: "x"})
	t.Logf("third-party plan here: %s", plan.Summary())

	rep, err := bootProbe(t, root, third, targets)
	if plan.Refused {
		if err == nil || !strings.Contains(err.Error(), "third-party") {
			t.Fatalf("this host cannot contain third-party; the boot must be refused naming the tier, got err=%v rep=%+v", err, rep)
		}
		return
	}
	if err != nil {
		t.Fatalf("a third-party manifest agent must boot under the default sandbox here: %v", err)
	}
	// The containment tier shows in the instance's scratch: <work_root>/third-party/….
	if !strings.Contains(filepath.ToSlash(rep.Temp), "/third-party/") {
		t.Errorf("the agent's scratch %q is not under the third-party tier", rep.Temp)
	}
	for _, addr := range targets {
		if rep.Dial[addr] == "ok" {
			t.Errorf("a third-party agent reached %s (want kernel-only): %v", addr, rep.Dial)
		}
	}

	// Control: the same agent with no declaration is first-party (normal
	// network). What it reaches shows which of the denials above discriminate.
	first := manifestAgentFromDisk(t, root, "netprobe_first_agent", "")
	if agentTrustTier(first) != agentTierFirstParty {
		t.Fatalf("an agent declaring no tier must be first-party: %+v", first)
	}
	frep, err := bootProbe(t, root, first, targets)
	if err != nil {
		t.Fatalf("first-party control: %v", err)
	}
	if !strings.Contains(filepath.ToSlash(frep.Temp), "/first-party/") {
		t.Errorf("control scratch %q is not under the first-party tier", frep.Temp)
	}
	reached := 0
	for _, addr := range targets {
		if frep.Dial[addr] == "ok" {
			reached++
		}
	}
	if reached == 0 {
		t.Logf("the first-party control reached none of %v here (no route, or the runner blocks it); "+
			"the third-party denials are then not discriminating on this host", targets)
	}
}
