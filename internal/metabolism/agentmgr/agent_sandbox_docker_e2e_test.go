package agentmgr

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/sandbox"
)

// ADR-0157 D2.1 end to end: the DOCKER runner for agents through the real
// launch path (InstanceManager.GetOrBootInstance → admitLaunch → buildAgentCmd
// → sandbox.StartCmd → `docker run` → socket wait → gRPC health → eviction →
// cleanup), for a first-party agent (--network host) and a third-party one
// (kernel-only: --network none, the kernel socket in the socket directory).
//
// The runner shares host paths and the agent's unix socket with the container
// at the same paths, so it needs a Linux host whose dockerd sees the kernel's
// filesystem (Docker Desktop's VM does not). It runs when
//
//	AGENTMGR_E2E_DOCKER_IMAGE  an image that can run this test binary (glibc:
//	                           e.g. debian:trixie-slim for a golang:1.25 build)
//
// is set; ENG-2017 ran it in a privileged golang:1.25 container with its own
// dockerd (docker-in-docker). It must pass as root AND as a non-root kernel:
// the agent (uid 65534) leaves a 0700 out/sub with a 0600 result in it, which
// a non-root kernel can collect and remove only after the runner's reclaim
// container hands the scratch back (ENG-2031). The out/ collection and the
// scratch removal are asserted on both.
func TestE2E_DockerRunnerAgentBootsConfined(t *testing.T) {
	image := os.Getenv("AGENTMGR_E2E_DOCKER_IMAGE")
	if image == "" || runtime.GOOS != "linux" {
		t.Skip("set AGENTMGR_E2E_DOCKER_IMAGE on a Linux docker host to run the docker runner for agents")
	}
	if testing.Short() {
		t.Skip("launches real containers; skipped in -short")
	}
	for _, tier := range []string{"first-party", "third-party"} {
		t.Run(tier, func(t *testing.T) { dockerRunnerProbe(t, image, tier) })
	}
}

func dockerRunnerProbe(t *testing.T, image, tier string) {
	root := e2eRoot(t)
	kernel := filepath.Join(root, "kernel")
	data := filepath.Join(root, "data")
	files := map[string]string{
		"config.db":  filepath.Join(kernel, "configs", "config.db"),
		"secret.key": filepath.Join(kernel, "configs", "secret.key"),
		"journal":    filepath.Join(data, "agents.db"),
		"vault":      filepath.Join(data, "vault", "blob"),
		// Not mounted: the host filesystem is invisible in the container.
		"unmounted": filepath.Join(root, "public", "readme.txt"),
	}
	for _, f := range files {
		mustWrite(t, f)
	}
	// Declared paths: the container's uid (65534) is not the kernel's, so they
	// are made writable for any uid; only the runner (a :ro mount) may deny.
	cache := filepath.Join(root, "caches", "model")
	weights := filepath.Join(root, "caches", "weights")
	for _, d := range []string{cache, weights} {
		agentOwnedDir(t, d)
		if err := os.Chmod(d, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	shared := filepath.Join(weights, "shared.txt")
	mustWrite(t, shared)
	files["declared-ro"] = shared
	// Hidden directories INSIDE a mounted one (a kernel-known runtime path),
	// one nested in the other like the kernel dir and its configs/: the runner
	// masks each again, empty and read-only (ADR-0157 D1). A tmpfs mounted
	// inside another tmpfs comes up 1777, so without :ro the inner one was
	// writable by the agent (ENG-2017).
	runtimeDir := filepath.Join(root, "runtime")
	outer := filepath.Join(runtimeDir, "state")
	nested := filepath.Join(outer, "inner")
	mustWrite(t, filepath.Join(nested, "token"))
	files["nested-state"] = filepath.Join(nested, "token")
	mustWrite(t, filepath.Join(runtimeDir, "lib.txt"))
	files["runtime-read"] = filepath.Join(runtimeDir, "lib.txt")
	workRoot := filepath.Join(root, "w")
	other := filepath.Join(workRoot, "first-party", "other-11112222", "secret.txt")
	mustWrite(t, other)
	files["other-agent"] = other

	sb := NewAgentSandbox(AgentSandboxOptions{
		Runner: sandbox.RunnerDocker, DockerImage: image,
		WorkRoot: workRoot, KernelDir: kernel,
		KernelStatePaths: []string{filepath.Join(kernel, "configs"), data, outer, nested},
		RuntimeReadPaths: []string{runtimeDir},
		AllowlistCap:     []string{filepath.Join(root, "caches")},
	})
	im := NewInstanceManager("127.0.0.1:1")
	im.SetAgentContainment(AgentContainment{Sandbox: sb})
	im.SetAgentTransport(AgentTransport{KernelSocket: listenKernelSocketForTest})
	im.SetManifestResolver(func(context.Context, string) *domain.AgentManifest {
		return &domain.AgentManifest{SandboxPaths: []domain.SandboxPath{{Path: cache, Write: true}, {Path: weights}}}
	})
	col := &recordingCollector{files: map[string]string{}}
	im.SetOutputCollector(col)
	var reads []string
	for _, f := range files {
		reads = append(reads, f)
	}
	confDir := filepath.Join(kernel, "configs")
	im.SetEnvExtras([]string{"AGENTMGR_CHAOS_HELPER=probe",
		"AGENTMGR_PROBE_READ=" + strings.Join(reads, string(os.PathListSeparator)),
		"AGENTMGR_PROBE_WRITE=" + strings.Join([]string{confDir, cache, weights, nested}, string(os.PathListSeparator))})
	// Binds under /tmp are dropped (the container's /tmp is private), and the
	// container uid must execute it: the probe runs from a 0755 copy.
	exe := copyProbeExe(t, root)
	id := "dprobe" + strings.ReplaceAll(tier, "-", "")
	def := &domain.AgentDefinition{ID: id, Runtime: domain.RuntimeBinary, ExecPath: exe,
		Dir: filepath.Dir(exe), TrustTier: tier}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	inst, err := im.GetOrBootInstance(ctx, def, "")
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	if err := waitHealthy(ctx, inst.SocketPath, 30*time.Second); err != nil {
		t.Fatalf("the kernel cannot reach the containerised agent over its socket: %v", err)
	}
	label := id + "-" + shortInstance(inst.ID) // the sandbox ServerID
	if n := agentContainers(t, label); n != 1 {
		t.Fatalf("want exactly one container labelled cambrian.agent=%s, got %d", label, n)
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

	for name, f := range files {
		want := "denied"
		if name == "declared-ro" || name == "runtime-read" {
			want = "ok"
		}
		if rep.Read[f] != want {
			t.Errorf("%s (%s): read %s, want %s", name, f, rep.Read[f], want)
		}
	}
	if rep.Scratch != "ok" {
		t.Errorf("the agent cannot write its own scratch: %s", rep.Scratch)
	}
	if rep.Write[cache] != "ok" {
		t.Errorf("the declared writable cache is not writable: %v", rep.Write)
	}
	if rep.Write[weights] != "denied" {
		t.Errorf("the declared read-only path was writable: %v", rep.Write)
	}
	if rep.Write[confDir] != "denied" {
		t.Errorf("the agent wrote into configs/: %v", rep.Write)
	}
	if rep.Write[nested] != "denied" {
		t.Errorf("the masked kernel-state directory inside a mount was writable: %v", rep.Write)
	}
	if _, err := os.Stat(filepath.Join(confDir, "probe-write.txt")); !os.IsNotExist(err) {
		t.Errorf("the agent's write reached the host configs/ directory: %v", err)
	}

	im.mu.Lock()
	pe := im.exits[inst.ID]
	im.mu.Unlock()
	scratchGlob := filepath.Join(workRoot, string(sandboxTier(def)), id+"-*")
	if m, _ := filepath.Glob(scratchGlob); len(m) != 1 {
		t.Fatalf("scratch dirs while running: %v", m)
	}
	im.EvictInstance(inst.ID)
	select {
	case <-pe.done:
	case <-time.After(60 * time.Second):
		t.Fatal("the evicted agent did not exit")
	}
	col.mu.Lock()
	got := col.files["sub/result.txt"]
	col.mu.Unlock()
	if got != "probe result" {
		t.Errorf("out/sub/result.txt (0600 in a 0700 dir of uid 65534) was not collected: %q (files %v)", got, col.files)
	}
	if m, _ := filepath.Glob(scratchGlob); len(m) != 0 {
		t.Errorf("scratch survived the exit: %v", m)
	}
	if n := reclaimContainers(t); n != 0 {
		t.Errorf("%d reclaim container(s) left behind", n)
	}
	if _, err := os.Stat(filepath.Dir(inst.SocketPath)); !os.IsNotExist(err) {
		t.Errorf("socket directory survived the exit: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for agentContainers(t, label) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the agent's container survived the eviction")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// reclaimContainers counts the reclaim containers (running or not) left.
func reclaimContainers(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("docker", "ps", "-aq", "--filter", "label=cambrian.sandbox=agent-reclaim").Output()
	if err != nil {
		t.Fatalf("docker ps: %v", err)
	}
	return len(strings.Fields(string(out)))
}

// agentContainers counts the containers (running or not) whose cambrian.agent
// label is the instance's sandbox server id.
func agentContainers(t *testing.T, label string) int {
	t.Helper()
	out, err := exec.Command("docker", "ps", "-aq", "--filter", "label=cambrian.agent="+label).Output()
	if err != nil {
		t.Fatalf("docker ps: %v", err)
	}
	return len(strings.Fields(string(out)))
}
