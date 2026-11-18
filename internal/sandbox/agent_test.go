package sandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// ADR-0157 S1: the non-stdio launch for kernel-spawned agents.

// agentPolicy is an agent-shaped policy: both tiers want write confinement and
// hidden kernel state (D1); first-party degrades, third-party refuses.
func agentPolicy(t *testing.T, hide ...string) *Policy {
	t.Helper()
	return &Policy{
		DefaultTier: TierThirdParty,
		Tiers: map[Tier]TierPolicy{
			TierFirstParty: {OnUnavailable: OnUnavailableDegrade, Runner: RunnerNative, ConfineFS: true, HideState: true},
			TierThirdParty: {OnUnavailable: OnUnavailableRefuse, Runner: RunnerNative, ConfineFS: true, HideState: true, LowPrivilege: true},
		},
		WorkRoot:   shortTempDir(t),
		WorkdirEnv: map[string]string{"TEMP": "tmp", "TMP": "tmp", "TMPDIR": "tmp", "HOME": "home"},
		HidePaths:  hide,
	}
}

// shortTempDir keeps socket paths under the 107-byte sockaddr_un limit.
func shortTempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "sbx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func agentSpec(p *Policy, tier Tier, sockDir string, binds ...Bind) Spec {
	return Spec{ServerID: "probe-0000aaaa", Tier: tier, TierSource: "agent:test", Policy: p,
		Agent: &AgentSpec{SocketDir: sockDir, Binds: binds, Ephemeral: true}}
}

func TestStartCmd_NeedsAnAgentSpec(t *testing.T) {
	_, _, err := StartCmd(context.Background(), Spec{ServerID: "x"}, Command{Path: "x"}, nil)
	if !errors.Is(err, ErrNotAgent) {
		t.Fatalf("err = %v, want ErrNotAgent", err)
	}
}

// The agent tiers want hide_state; the MCP tiers do not, so an MCP server's
// report is unchanged by ADR-0157.
func TestHideStateWantedByAgentTiersOnly(t *testing.T) {
	ap := agentPolicy(t)
	rep := Plan(context.Background(), agentSpec(ap, TierFirstParty, ""), Command{Path: "x"})
	all := append(controlStrings(rep.Enforced), rep.Missing...)
	if !slices.ContainsFunc(all, func(s string) bool { return strings.HasPrefix(s, string(ControlHideState)) }) {
		t.Fatalf("agent report does not mention hide_state: %+v", rep)
	}
	mp := testPolicy(t)
	mrep := Plan(context.Background(), mp.Resolve("srv", "third-party", "", false, ServerSpec{}), Command{Path: "x"})
	for _, s := range append(controlStrings(mrep.Enforced), mrep.Missing...) {
		if strings.HasPrefix(s, string(ControlHideState)) {
			t.Fatalf("MCP report gained hide_state: %+v", mrep)
		}
	}
	if runtime.GOOS == "windows" {
		// ADR-0157 S5 (D2.4): an agent runs in the AppContainer, which hides
		// the kernel's state; the report says so, with the mechanism.
		if !slices.Contains(rep.Enforced, ControlHideState) || !slices.Contains(rep.Mechanisms, MechanismAppContainer) ||
			rep.Degraded() || rep.Refused {
			t.Fatalf("Windows agent report must enforce hide_state through the AppContainer: %s", rep.Summary())
		}
	}
}

func controlStrings(cs []Control) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = string(c)
	}
	return out
}

// A tier that refuses a missing control starts nothing.
func TestStartCmd_RefusesWhenTheTierRefuses(t *testing.T) {
	p := agentPolicy(t)
	spec := agentSpec(p, TierThirdParty, "")
	rep := Plan(context.Background(), spec, Command{Path: "x"})
	if len(rep.Missing) == 0 {
		t.Skip("every third-party agent control is enforced on this host")
	}
	l, _, err := StartCmd(context.Background(), spec, fixture(t, "sleep"), nil)
	var refused *RefusedError
	if !errors.As(err, &refused) || l != nil {
		t.Fatalf("want a refusal, got l=%v err=%v", l, err)
	}
	if _, serr := os.Stat(WorkdirFor(p, spec)); serr == nil {
		t.Fatal("a refused launch created its workdir")
	}
}

// The caller owns the pipes and the Wait; Release kills the tree, frees the OS
// resources (the Windows job watcher: goleak in TestMain) and removes the
// ephemeral scratch.
func TestStartCmd_CallerPipesWaitAndRelease(t *testing.T) {
	p := agentPolicy(t)
	sockDir := filepath.Join(p.WorkRoot, "sock", "0000aaaa")
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	codeDir := t.TempDir()
	c := fixture(t, "env")
	c.Dir = codeDir
	if CoversPrivateTmp(filepath.Dir(c.Path)) {
		// A test binary directly in /tmp (or /) cannot be shown at its own
		// directory (ENG-1969); show a copy from a subdirectory instead.
		c.Path = copyExecutable(t, c.Path)
	}
	// The fixture binary and the code dir are shown at their own paths, as
	// agentmgr shows an agent's code and runtime (bubblewrap's /tmp is private).
	// A bind of / is never made over the private /tmp: dropped and reported.
	spec := agentSpec(p, TierFirstParty, sockDir, Bind{Path: filepath.Dir(c.Path)}, Bind{Path: codeDir}, Bind{Path: "/"})
	var stdout io.ReadCloser
	l, rep, err := StartCmd(context.Background(), spec, c, func(cmd *exec.Cmd) error {
		var perr error
		stdout, perr = cmd.StdoutPipe()
		return perr
	})
	if err != nil {
		t.Fatalf("start: %v (report %+v)", err, rep)
	}
	if !strings.Contains(strings.Join(rep.Notes, ";"), "bind / skipped") {
		t.Errorf("the dropped / bind is not reported: %v", rep.Notes)
	}
	if len(spec.Agent.Binds) != 3 {
		t.Errorf("StartCmd changed the caller's binds: %+v", spec.Agent.Binds)
	}
	out, _ := io.ReadAll(stdout)
	_ = l.Cmd.Wait()
	l.Release()
	l.Release() // idempotent

	lines := string(out)
	inner := l.Workdir()
	if strings.Contains(strings.Join(rep.Mechanisms, ","), "bwrap") {
		inner = "/tmp/work" // bwrapWork (unix-only const)
	}
	for _, k := range []string{"TEMP", "TMPDIR", "HOME"} {
		if !strings.Contains(lines, k+"="+inner) {
			t.Errorf("%s does not point into the scratch dir %s:\n%s", k, inner, lines)
		}
	}
	if !strings.Contains(strings.ToLower(lines), "cwd="+strings.ToLower(codeDir)) {
		t.Errorf("cwd is not the agent's code dir %s:\n%s", codeDir, lines)
	}
	if _, err := os.Stat(l.Workdir()); !os.IsNotExist(err) {
		t.Fatalf("scratch %s survives Release: %v", l.Workdir(), err)
	}
}

func TestSplitBindsOrdersAroundTheHideList(t *testing.T) {
	root := t.TempDir()
	kernel := filepath.Join(root, "kernel")
	code := filepath.Join(kernel, "agents")
	cache := filepath.Join(root, "cache")
	for _, d := range []string{code, cache} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	before, after, missing := splitBinds([]Bind{{Path: cache, Write: true}, {Path: code}, {Path: filepath.Join(root, "nope")}},
		[]string{kernel})
	if len(before) != 1 || before[0].Path != cache || len(after) != 1 || after[0].Path != code || len(missing) != 1 {
		t.Fatalf("before=%v after=%v missing=%v", before, after, missing)
	}
	if within(filepath.Join(root, "kernelx"), kernel) || !within(code, kernel) || !within(kernel, kernel) {
		t.Fatal("within is not a path-segment prefix test")
	}
}

// The docker runner for agents: a separate non-root uid, only declared paths
// mounted, a hidden path inside a mount masked again, secrets by NAME only.
func TestDockerAgentArgs(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	conf := filepath.Join(home, "kernel", "configs")
	workdir := filepath.Join(root, "work")
	sock := filepath.Join(root, "sock")
	for _, d := range []string{conf, workdir, sock} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	p := agentPolicy(t, conf)
	spec := agentSpec(p, TierFirstParty, sock, Bind{Path: home})
	spec.Server.Image = "cambrian/kernel:test"
	c := Command{Path: "/opt/venv/bin/python", Args: []string{"agent.py", "--socket", sock + "/a.sock"},
		Env:     []string{"PATH=/usr/bin", "CAMBRIAN_AGENT_TOKEN=s3cr3t"},
		Forward: []string{"CAMBRIAN_AGENT_TOKEN"}, Dir: "/srv/agents"}
	args, cliEnv, err := dockerAgentArgs(p, spec, p.Tiers[TierFirstParty], c, workdir)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"--user 65534:65534", "--network host", "--read-only", "--cap-drop ALL",
		"-v " + home + ":" + home + ":ro", "-v " + sock + ":" + sock, "-v " + workdir + ":/work", "--tmpfs " + conf + ":ro",
		"-w /srv/agents", "-e CAMBRIAN_AGENT_TOKEN", "cambrian/kernel:test /bin/sh -c"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv lacks %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "s3cr3t") || strings.Contains(joined, "PATH=/usr/bin") {
		t.Fatalf("a secret or a host OS variable is on argv: %s", joined)
	}
	if !slices.Contains(cliEnv, "CAMBRIAN_AGENT_TOKEN=s3cr3t") {
		t.Fatal("the forwarded value is not in the docker CLI's own environment")
	}
	// ADR-0157 S2: a kernel-only tier runs with --network none; the socket
	// directory (the kernel socket's home) stays mounted at its own path.
	kp := p.Tiers[TierFirstParty]
	kp.DenyNetwork = true
	args, _, err = dockerAgentArgs(p, spec, kp, c, workdir)
	if err != nil {
		t.Fatal(err)
	}
	joined = strings.Join(args, " ")
	if !strings.Contains(joined, "--network none") || strings.Contains(joined, "--network host") ||
		!strings.Contains(joined, "-v "+sock+":"+sock) {
		t.Fatalf("kernel-only docker argv: %s", joined)
	}
}

// Docker for agents needs a Linux host: elsewhere it is unusable, and a
// degrade tier falls back to native.
func TestDockerAgentRunnerNeedsLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("Linux host")
	}
	ap := agentPolicy(t)
	capab := dockerAgentCapability(context.Background(), ap, agentSpec(ap, TierFirstParty, ""), ap.Tiers[TierFirstParty], Command{Path: "x"})
	if !strings.Contains(capab.unusable, "needs a Linux host") {
		t.Fatalf("unusable = %q", capab.unusable)
	}
}

// ENG-1969: a bind over the agent's private /tmp ("/", "/tmp", "/tmp/work") is
// never made; anything below /tmp other than the workdir mount is fine.
func TestCoversPrivateTmp(t *testing.T) {
	for p, want := range map[string]bool{
		"/": true, "/tmp": true, "/tmp/": true, "/tmp/work": true, "/tmp/../tmp": true,
		"/tmp/bin": false, "/tmp/work/x": false, "/usr": false, "/tmpx": false, "": false,
	} {
		if got := CoversPrivateTmp(p); got != want {
			t.Errorf("CoversPrivateTmp(%q) = %v, want %v", p, got, want)
		}
	}
	kept, skipped := privateTmpSafe([]Bind{{Path: "/tmp"}, {Path: "/opt/venv"}, {Path: "/", Write: true}, {Path: "/tmp/bin"}})
	if len(kept) != 2 || kept[0].Path != "/opt/venv" || kept[1].Path != "/tmp/bin" || len(skipped) != 2 {
		t.Fatalf("kept %+v skipped %v", kept, skipped)
	}
}

// copyExecutable copies the test binary into a fresh subdirectory.
func copyExecutable(t *testing.T, src string) string {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "bin", filepath.Base(src))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		t.Fatal(err)
	}
	return dst
}
