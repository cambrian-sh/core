package agentmgr

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/sandbox"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// ADR-0157 S1 end to end through the REAL agent launch path:
// InstanceManager.GetOrBootInstance → admitLaunch → buildAgentCmd →
// sandbox.StartCmd → socket wait → gRPC health → eviction → cleanup.
//
// The agent is this test binary in its "probe" shape (TestMain): it tries to
// read the kernel's state and to write outside its scratch, writes what it
// could reach into probe.json in its socket directory, and serves
// grpc.health.v1 on the socket the kernel named — so the kernel↔agent
// transport through the sandbox is exercised too.
//
// What it asserts depends on what this platform can enforce, and that is the
// point of the honest boot log (D2.5):
//   - Linux with bubblewrap: config.db, secret.key, the journal, the vault and
//     another instance's scratch are unreadable; a path outside the hide-list
//     is readable (the control);
//   - Windows (Job Object + low integrity): writes outside the scratch are
//     denied, reads are NOT, and the report says so (hide_state missing).

type probeReport struct {
	Read    map[string]string `json:"read"`
	Write   map[string]string `json:"write"`
	Scratch string            `json:"scratch"`
	Temp    string            `json:"temp"`
	Cwd     string            `json:"cwd"`
}

// runProbeAgent is the probe agent (TestMain, AGENTMGR_CHAOS_HELPER=probe).
func runProbeAgent() {
	var sock string
	for i, a := range os.Args {
		if a == "--socket" && i+1 < len(os.Args) {
			sock = os.Args[i+1]
		}
	}
	rep := probeReport{Read: map[string]string{}, Write: map[string]string{}}
	for _, p := range filepath.SplitList(os.Getenv("AGENTMGR_PROBE_READ")) {
		if _, err := os.ReadFile(p); err == nil {
			rep.Read[p] = "ok"
		} else {
			rep.Read[p] = "denied"
		}
	}
	for _, d := range filepath.SplitList(os.Getenv("AGENTMGR_PROBE_WRITE")) {
		if err := os.WriteFile(filepath.Join(d, "probe-write.txt"), []byte("x"), 0o600); err == nil {
			rep.Write[d] = "ok"
		} else {
			rep.Write[d] = "denied"
		}
	}
	rep.Temp = os.TempDir()
	if err := os.WriteFile(filepath.Join(rep.Temp, "scratch.txt"), []byte("x"), 0o600); err == nil {
		rep.Scratch = "ok"
	} else {
		rep.Scratch = "denied: " + err.Error()
	}
	rep.Cwd, _ = os.Getwd()
	b, _ := json.Marshal(rep)
	if err := os.WriteFile(filepath.Join(filepath.Dir(sock), "probe.json"), b, 0o600); err != nil {
		os.Exit(4)
	}
	lis, err := net.Listen("unix", sock)
	if err != nil {
		os.Exit(3)
	}
	srv := grpc.NewServer()
	healthpb.RegisterHealthServer(srv, health.NewServer())
	_ = srv.Serve(lis)
}

func TestE2E_SandboxedAgentBootsConfined(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a real subprocess; skipped in -short")
	}
	root := e2eRoot(t)
	kernel := filepath.Join(root, "kernel")
	data := filepath.Join(root, "data")
	files := map[string]string{
		"config.db":  filepath.Join(kernel, "configs", "config.db"),
		"secret.key": filepath.Join(kernel, "configs", "secret.key"),
		"journal":    filepath.Join(data, "agents.db"),
		"vault":      filepath.Join(data, "vault", "blob"),
		"visible":    filepath.Join(root, "public", "readme.txt"),
	}
	for _, f := range files {
		mustWrite(t, f)
	}
	cache := filepath.Join(root, "caches", "model")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	workRoot := filepath.Join(root, "w")
	other := filepath.Join(workRoot, "first-party", "other-11112222", "secret.txt")
	mustWrite(t, other)
	files["other-agent"] = other

	sb := NewAgentSandbox(AgentSandboxOptions{
		WorkRoot: workRoot, KernelDir: kernel,
		KernelStatePaths: []string{filepath.Join(kernel, "configs"), data},
		AllowlistCap:     []string{filepath.Join(root, "caches")},
	})
	im := NewInstanceManager("", "127.0.0.1:1")
	im.SetAgentContainment(AgentContainment{Sandbox: sb})
	im.SetManifestResolver(func(context.Context, string) *domain.AgentManifest {
		return &domain.AgentManifest{SandboxPaths: []domain.SandboxPath{{Path: cache, Write: true}}}
	})
	var reads []string
	for _, f := range files {
		reads = append(reads, f)
	}
	confDir := filepath.Join(kernel, "configs")
	im.SetEnvExtras([]string{"AGENTMGR_CHAOS_HELPER=probe",
		"AGENTMGR_PROBE_READ=" + strings.Join(reads, string(os.PathListSeparator)),
		"AGENTMGR_PROBE_WRITE=" + strings.Join([]string{confDir, cache}, string(os.PathListSeparator))})
	exe, _ := os.Executable()
	def := &domain.AgentDefinition{ID: "probe", Runtime: domain.RuntimeBinary, ExecPath: exe,
		Dir: filepath.Dir(exe), TrustTier: "first-party"}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	inst, err := im.GetOrBootInstance(ctx, def, "")
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	if !withinPath(inst.SocketPath, workRoot) {
		t.Fatalf("socket %s is not under the agent work root", inst.SocketPath)
	}
	if err := waitHealthy(ctx, inst.SocketPath, 10*time.Second); err != nil {
		t.Fatalf("the kernel cannot reach the confined agent over its socket: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(inst.SocketPath), "probe.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rep probeReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	t.Logf("probe: %s", raw)

	if rep.Scratch != "ok" {
		t.Errorf("the agent cannot write its own scratch: %s", rep.Scratch)
	}
	if rep.Write[cache] != "ok" {
		t.Errorf("the declared writable cache is not writable: %v", rep.Write)
	}
	switch {
	case runtime.GOOS == "windows":
		// Low integrity: writes up are denied; reads are not confined, and the
		// report must say so.
		if rep.Write[confDir] != "denied" {
			t.Errorf("a low-integrity agent wrote into configs/: %v", rep.Write)
		}
		plan := planAgent(sb)
		if !slices.ContainsFunc(plan.Missing, func(m string) bool {
			return strings.HasPrefix(m, "hide_state:") && strings.Contains(m, "NOT confined")
		}) || !slices.Contains(plan.Enforced, sandbox.ControlFSConfine) || plan.Refused {
			t.Errorf("Windows report must enforce fs_confine and name reads NOT confined: %s", plan.Summary())
		}
		if rep.Read[files["config.db"]] != "ok" {
			t.Logf("note: config.db unreadable at low integrity here (%s)", rep.Read[files["config.db"]])
		}
	case bwrapHere():
		for name, f := range files {
			want := "denied"
			if name == "visible" {
				want = "ok"
			}
			if rep.Read[f] != want {
				t.Errorf("%s (%s): read %s, want %s", name, f, rep.Read[f], want)
			}
		}
		if rep.Write[confDir] != "denied" {
			t.Errorf("the agent wrote into configs/: %v", rep.Write)
		}
		if !slices.Contains(planAgent(sb).Enforced, sandbox.ControlHideState) {
			t.Errorf("hide_state not reported enforced")
		}
	default:
		t.Logf("no read confinement on this host (%s); the report names it missing", runtime.GOOS)
	}

	// Whatever the platform, nothing the agent wrote reached the host's configs/.
	if _, err := os.Stat(filepath.Join(confDir, "probe-write.txt")); !os.IsNotExist(err) {
		t.Errorf("the agent's write reached the host configs/ directory: %v", err)
	}

	// The per-instance directories are removed when the process exits.
	im.mu.Lock()
	pe := im.exits[inst.ID]
	im.mu.Unlock()
	scratchGlob := filepath.Join(workRoot, "first-party", "probe-*")
	if m, _ := filepath.Glob(scratchGlob); len(m) != 1 {
		t.Fatalf("scratch dirs while running: %v", m)
	}
	im.EvictInstance(inst.ID)
	select {
	case <-pe.done:
	case <-time.After(30 * time.Second):
		t.Fatal("the evicted agent did not exit")
	}
	if m, _ := filepath.Glob(scratchGlob); len(m) != 0 {
		t.Fatalf("scratch survived the exit: %v", m)
	}
	if _, err := os.Stat(filepath.Dir(inst.SocketPath)); !os.IsNotExist(err) {
		t.Fatalf("socket directory survived the exit: %v", err)
	}
}

// The real path refuses a declared path the operator cap does not admit,
// naming it, before anything is spawned.
func TestE2E_SandboxRefusesAnUndeclaredPathAtBoot(t *testing.T) {
	root := e2eRoot(t)
	sb := NewAgentSandbox(AgentSandboxOptions{WorkRoot: filepath.Join(root, "w"), AllowlistCap: []string{filepath.Join(root, "caches")}})
	im := NewInstanceManager("", "127.0.0.1:1")
	im.SetAgentContainment(AgentContainment{Sandbox: sb})
	bad := filepath.Join(root, "home", ".ssh")
	im.SetManifestResolver(func(context.Context, string) *domain.AgentManifest {
		return &domain.AgentManifest{SandboxPaths: []domain.SandboxPath{{Path: bad}}}
	})
	def := noSocketAgent()
	_, err := im.GetOrBootInstance(context.Background(), def, "")
	if err == nil || !strings.Contains(err.Error(), bad) || !strings.Contains(err.Error(), "allowlist_cap") {
		t.Fatalf("want a refusal naming %s, got %v", bad, err)
	}
	assertNothingRegistered(t, im, def)
	if m, _ := filepath.Glob(filepath.Join(root, "w", "first-party", "*")); len(m) != 0 {
		t.Fatalf("a refused boot left a scratch dir: %v", m)
	}
}

func mustWrite(t *testing.T, f string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// e2eRoot is a short directory outside /tmp: bubblewrap gives the agent its own
// /tmp (a /tmp fixture would be hidden for the wrong reason), and socket paths
// must stay under 107 bytes. $AGENTMGR_E2E_ROOT wins when set.
func e2eRoot(t *testing.T) string {
	t.Helper()
	base := os.Getenv("AGENTMGR_E2E_ROOT")
	if base == "" && runtime.GOOS == "linux" {
		if c, err := os.UserCacheDir(); err == nil {
			base = c
		}
	}
	if base != "" {
		if err := os.MkdirAll(base, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	d, err := os.MkdirTemp(base, "ae")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// bwrapHere reports whether the agent sandbox's Linux runner can use
// bubblewrap on this host.
func bwrapHere() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	return slices.Contains(planAgent(NewAgentSandbox(AgentSandboxOptions{})).Enforced, sandbox.ControlHideState)
}

// planAgent is the containment a first-party agent gets on this host.
func planAgent(sb *AgentSandbox) sandbox.Report {
	return sandbox.Plan(context.Background(), sandbox.Spec{ServerID: "plan", Tier: sandbox.TierFirstParty,
		Policy: sb.Policy(), Agent: &sandbox.AgentSpec{}}, sandbox.Command{Path: "x"})
}
