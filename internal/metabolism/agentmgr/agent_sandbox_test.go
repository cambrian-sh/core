package agentmgr

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/procenv"
	"github.com/cambrian-sh/core/internal/sandbox"
)

// ADR-0157 S1 unit tests: the spec built from tier + allowlist, the refused
// declared path, disabled ⇒ unchanged argv/env, the boot-log report.

func testAgentSandbox(t *testing.T, mut func(*AgentSandboxOptions)) (*AgentSandbox, string) {
	t.Helper()
	root, err := os.MkdirTemp("", "asb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	o := AgentSandboxOptions{
		WorkRoot:         filepath.Join(root, "work"),
		KernelDir:        filepath.Join(root, "kernel"),
		KernelStatePaths: []string{filepath.Join(root, "kernel", "configs"), filepath.Join(root, "data")},
		HidePaths:        []string{filepath.Join(root, "pgdata")},
		RuntimeReadPaths: []string{filepath.Join(root, "kernel", "prompts")},
		AllowlistCap:     []string{filepath.Join(root, "caches")},
	}
	if mut != nil {
		mut(&o)
	}
	return NewAgentSandbox(o), root
}

func pythonDef() *domain.AgentDefinition {
	return &domain.AgentDefinition{ID: "worker", Runtime: domain.RuntimePython, ExecPath: "worker_agent.py",
		Dir: "agents", LaunchArgs: []string{"--profile", "researcher", "--agent-id", "researcher"}}
}

// Disabled (no sandbox): argv and env are exactly what the launch built before
// ADR-0157 — socket under the OS temp directory, the SEC-01 env.
func TestBuildAgentCmd_DisabledIsUnchanged(t *testing.T) {
	im := withPython(NewInstanceManager("127.0.0.1:50051"), "/opt/venv/bin/python")
	im.SetEnvExtras([]string{"CAMBRIAN_TOOL_MENU_K=8"})
	def, inst := pythonDef(), domain.NewInstance("worker")
	cmd, err := im.buildAgentCmd(def, inst, "")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{"/opt/venv/bin/python", "worker_agent.py", "--socket", socketPath(def.ID, inst.ID),
		"--substrate-addr", "127.0.0.1:50051", "--profile", "researcher", "--agent-id", "researcher"}
	if !slices.Equal(cmd.Args, wantArgs) {
		t.Fatalf("argv\n got %q\nwant %q", cmd.Args, wantArgs)
	}
	wantEnv := append(procenv.Allowlisted(nil), "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1", "CAMBRIAN_TOOL_MENU_K=8")
	if !slices.Equal(cmd.Env, wantEnv) {
		t.Fatalf("env\n got %q\nwant %q", cmd.Env, wantEnv)
	}
	if filepath.Dir(socketPath(def.ID, inst.ID)) != filepath.Clean(os.TempDir()) {
		t.Fatal("the unsandboxed socket left the OS temp directory")
	}
}

// Enabled: the same argv and env; only the socket moves into the instance's
// socket directory under the sandbox work root (D2.6).
func TestBuildAgentCmd_SandboxedMovesOnlyTheSocket(t *testing.T) {
	sb, _ := testAgentSandbox(t, nil)
	plain := withPython(NewInstanceManager("127.0.0.1:50051"), "/opt/venv/bin/python")
	boxed := withPython(NewInstanceManager("127.0.0.1:50051"), "/opt/venv/bin/python")
	boxed.SetAgentContainment(AgentContainment{Sandbox: sb})
	def, inst := pythonDef(), domain.NewInstance("worker")
	a, _ := plain.buildAgentCmd(def, inst, "")
	b, _ := boxed.buildAgentCmd(def, inst, "")
	if !slices.Equal(a.Env, b.Env) {
		t.Fatalf("env changed:\n%q\n%q", a.Env, b.Env)
	}
	want := slices.Clone(a.Args)
	want[3] = filepath.Join(sb.socketDir(inst.ID), filepath.Base(a.Args[3]))
	if !slices.Equal(b.Args, want) {
		t.Fatalf("argv\n got %q\nwant %q", b.Args, want)
	}
	if !withinPath(b.Args[3], sb.Policy().WorkRoot) {
		t.Fatalf("socket %s outside the work root", b.Args[3])
	}
}

func TestAdmitDeclared(t *testing.T) {
	sb, root := testAgentSandbox(t, nil)
	cache := filepath.Join(root, "caches", "hf")
	got, err := sb.admitDeclared("docling", []domain.SandboxPath{{Path: cache, Write: true}})
	if err != nil || len(got) != 1 || got[0].Path != cache || !got[0].Write {
		t.Fatalf("admitted path: %v %v", got, err)
	}
	for name, p := range map[string]string{
		"outside the cap":   filepath.Join(root, "elsewhere"),
		"relative":          "models/hf",
		"inside configs":    filepath.Join(root, "kernel", "configs", "x"),
		"inside work root":  filepath.Join(root, "work", "first-party", "other"),
		"inside hide_paths": filepath.Join(root, "pgdata", "base"),
	} {
		_, err := sb.admitDeclared("docling", []domain.SandboxPath{{Path: p}})
		if !errors.Is(err, ErrSandboxPathRefused) || !strings.Contains(err.Error(), p) {
			t.Errorf("%s: want a refusal naming %q, got %v", name, p, err)
		}
	}
	// A cap entry that would admit the kernel's state still cannot.
	sb2, root2 := testAgentSandbox(t, func(o *AgentSandboxOptions) { o.AllowlistCap = []string{string(filepath.Separator)} })
	if _, err := sb2.admitDeclared("x", []domain.SandboxPath{{Path: filepath.Join(root2, "data", "vault")}}); !errors.Is(err, ErrSandboxPathRefused) {
		t.Fatalf("the data dir was admitted through a root cap: %v", err)
	}
}

// The spec: tier from the kernel's view, binds = code + runtime prefix +
// kernel-known read paths + admitted manifest paths, memory from admission.
func TestSandboxLaunch_SpecFromTierAndAllowlist(t *testing.T) {
	sb, root := testAgentSandbox(t, nil)
	venv := filepath.Join(root, "venv")
	py := filepath.Join(venv, "bin", "python")
	if runtime.GOOS == "windows" {
		py = filepath.Join(venv, "Scripts", "python.exe")
	}
	im := withPython(NewInstanceManager("127.0.0.1:1"), py)
	im.SetAgentContainment(AgentContainment{Sandbox: sb})
	cache := filepath.Join(root, "caches", "hf")
	im.SetManifestResolver(func(context.Context, string) *domain.AgentManifest {
		return &domain.AgentManifest{SandboxPaths: []domain.SandboxPath{{Path: cache, Write: true}}}
	})
	for _, tc := range []struct {
		def  *domain.AgentDefinition
		tier sandbox.Tier
	}{
		{&domain.AgentDefinition{ID: "kg_extractor", Runtime: domain.RuntimePython, ExecPath: "agent.py", Dir: "agents", System: true}, sandbox.TierFirstParty},
		{&domain.AgentDefinition{ID: "researcher", Runtime: domain.RuntimePython, ExecPath: "w.py", Dir: "agents", TrustTier: "first-party"}, sandbox.TierFirstParty},
		{&domain.AgentDefinition{ID: "vendor", Runtime: domain.RuntimePython, ExecPath: "w.py", Dir: "agents", TrustTier: "third-party"}, sandbox.TierThirdParty},
	} {
		inst := domain.NewInstance(tc.def.ID)
		cmd, err := im.buildAgentCmd(tc.def, inst, "")
		if err != nil {
			t.Fatal(err)
		}
		cmd.Dir = tc.def.Dir
		spec, c, kernelSock, err := im.sandboxLaunch(context.Background(), tc.def, inst, cmd, 1536)
		if err != nil {
			t.Fatal(err)
		}
		if spec.Tier != tc.tier || spec.Server.MemoryMB != 1536 || spec.Agent == nil || !spec.Agent.Ephemeral {
			t.Fatalf("%s: spec %+v", tc.def.ID, spec)
		}
		absDir, _ := filepath.Abs("agents")
		var paths []string
		for _, b := range spec.Agent.Binds {
			paths = append(paths, b.Path)
		}
		for _, want := range []string{absDir, venv, filepath.Join(root, "kernel", "prompts"), cache} {
			if !slices.Contains(paths, want) {
				t.Errorf("%s: binds %v lack %s", tc.def.ID, paths, want)
			}
		}
		if spec.Agent.Binds[len(spec.Agent.Binds)-1] != (sandbox.Bind{Path: cache, Write: true, Private: true}) || spec.Agent.Identity != tc.def.ID {
			t.Errorf("%s: the declared cache is not writable: %+v", tc.def.ID, spec.Agent.Binds)
		}
		wantArgs := cmd.Args[1:]
		if runtime.GOOS != "windows" && kernelSock != (tc.tier == sandbox.TierThirdParty) {
			t.Errorf("%s: kernel socket transport = %v", tc.def.ID, kernelSock)
		}
		if kernelSock {
			// ADR-0157 S2: a kernel-only instance dials its kernel socket; only
			// the --substrate-addr VALUE changes.
			wantArgs = slices.Clone(wantArgs)
			i := slices.Index(wantArgs, "--substrate-addr")
			wantArgs[i+1] = "unix:" + filepath.Join(spec.Agent.SocketDir, kernelSocketName)
		}
		if !slices.Equal(c.Args, wantArgs) || !slices.Equal(c.Env, cmd.Env) || c.Dir != absDir {
			t.Errorf("%s: command changed: %+v", tc.def.ID, c)
		}
		if slices.Contains(c.Forward, "PATH") {
			t.Errorf("%s: a host OS variable is forwarded: %v", tc.def.ID, c.Forward)
		}
	}
}

// ENG-1969: an interpreter in /tmp/bin (or /bin) has its own directory bound,
// never /tmp or /, which would cover the sandbox's private /tmp; a real venv
// root is bound as before.
func TestRuntimePrefixAndBindsNeverCoverThePrivateTmp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("slash paths: the private /tmp is a bubblewrap/docker property")
	}
	for exe, want := range map[string]string{
		"/tmp/bin/python":      "/tmp/bin",
		"/bin/python":          "/bin",
		"/opt/venv/bin/python": "/opt/venv",
		"/tmp/tb/am.test":      "/tmp/tb",
		"/usr/local/bin/node":  "/usr/local",
	} {
		if got := runtimePrefix(exe); got != want {
			t.Errorf("runtimePrefix(%q) = %q, want %q", exe, got, want)
		}
	}
	sb, _ := testAgentSandbox(t, nil)
	im := withPython(NewInstanceManager("127.0.0.1:1"), "/tmp/bin/python")
	im.SetAgentContainment(AgentContainment{Sandbox: sb})
	binds, err := im.agentBinds(context.Background(), pythonDef(), "/tmp/bin/python")
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, b := range binds {
		if sandbox.CoversPrivateTmp(b.Path) {
			t.Fatalf("bind %s covers the private /tmp: %+v", b.Path, binds)
		}
		paths = append(paths, b.Path)
	}
	if !slices.Contains(paths, "/tmp/bin") {
		t.Fatalf("the interpreter's own directory is not bound: %v", paths)
	}
}

// The tiers want D1 everywhere; the hide-list is the kernel dir + state +
// operator additions + the agent work root, and never only the additions.
func TestNewAgentSandbox_PolicyAndHideList(t *testing.T) {
	sb, root := testAgentSandbox(t, func(o *AgentSandboxOptions) { o.FirstPartyRefuse = true })
	p := sb.Policy()
	for _, want := range []string{"kernel", filepath.Join("kernel", "configs"), "data", "pgdata", "work"} {
		if !slices.Contains(p.HidePaths, filepath.Join(root, want)) {
			t.Errorf("hide list %v lacks %s", p.HidePaths, want)
		}
	}
	fp, tp := p.Tiers[sandbox.TierFirstParty], p.Tiers[sandbox.TierThirdParty]
	if !fp.ConfineFS || !fp.HideState || fp.DenyNetwork || fp.OnUnavailable != sandbox.OnUnavailableRefuse {
		t.Errorf("first-party tier: %+v", fp)
	}
	if !tp.ConfineFS || !tp.HideState || !tp.LowPrivilege || !tp.DenyNetwork || tp.OnUnavailable != sandbox.OnUnavailableDegrade {
		t.Errorf("third-party tier (ADR-0157 D3: kernel-only): %+v", tp)
	}
	if got := sb.KernelOnlyTiers(); !slices.Equal(got, []string{string(sandbox.TierThirdParty)}) {
		t.Errorf("per_tier kernel-only tiers = %v", got)
	}
	if def := NewAgentSandbox(AgentSandboxOptions{ThirdPartyRefuse: true}).Policy(); def.Tiers[sandbox.TierFirstParty].OnUnavailable != sandbox.OnUnavailableDegrade || def.WorkRoot == "" {
		t.Errorf("defaults: first-party %q (Q2: degrade), work root %q", def.Tiers[sandbox.TierFirstParty].OnUnavailable, def.WorkRoot)
	}
}

// ADR-0157 S7 live run: the user profile moves into the scratch on Windows
// (USERPROFILE/APPDATA/LOCALAPPDATA, which Path.home() and the cache libraries
// read), as HOME does everywhere; Unix gets no Windows variables.
func TestAgentWorkdirEnv_ProfileInScratch(t *testing.T) {
	win := agentWorkdirEnv("windows")
	for k, want := range map[string]string{"HOME": "home", "USERPROFILE": "home", "APPDATA": "home/AppData/Roaming",
		"LOCALAPPDATA": "home/AppData/Local", "TEMP": "tmp", "TMP": "tmp", AgentOutEnv: AgentOutDir} {
		if win[k] != want {
			t.Errorf("windows %s = %q, want %q", k, win[k], want)
		}
	}
	lin := agentWorkdirEnv("linux")
	for _, k := range []string{"USERPROFILE", "APPDATA", "LOCALAPPDATA"} {
		if _, ok := lin[k]; ok {
			t.Errorf("linux env sets %s", k)
		}
	}
	if lin["HOME"] != "home" || lin["TMPDIR"] != "tmp" || lin[AgentOutEnv] != AgentOutDir {
		t.Errorf("linux env: %v", lin)
	}
	if got := NewAgentSandbox(AgentSandboxOptions{}).Policy().WorkdirEnv; got["HOME"] != "home" ||
		(runtime.GOOS == "windows") != (got["USERPROFILE"] == "home") {
		t.Errorf("policy WorkdirEnv on %s: %v", runtime.GOOS, got)
	}
}

// ADR-0157 S2: with the sandbox on, a third-party agent is ADMITTED with the
// tier memory cap — whether it starts is the sandbox plan's decision over the
// controls the runner actually has (D1 + D3). Without the sandbox it is still
// refused, naming the missing controls.
func TestAdmitLaunch_ThirdPartyWithSandboxIsAdmitted(t *testing.T) {
	sb, _ := testAgentSandbox(t, nil)
	im := withPython(NewInstanceManager(""), "python")
	im.SetAgentContainment(AgentContainment{Sandbox: sb, ThirdPartyMemoryMB: 768})
	mem, err := im.admitLaunch(context.Background(), &domain.AgentDefinition{ID: "vendor", Runtime: domain.RuntimePython, TrustTier: "third-party"})
	if err != nil || mem != 768 {
		t.Fatalf("admit = %d, %v", mem, err)
	}
	bare := withPython(NewInstanceManager(""), "python")
	if _, err := bare.admitLaunch(context.Background(), &domain.AgentDefinition{ID: "vendor", TrustTier: "third-party"}); err == nil ||
		!strings.Contains(err.Error(), "network") || !strings.Contains(err.Error(), "fs_confine") {
		t.Fatalf("unsandboxed third-party: err = %v", err)
	}
}

// kernel_only_all: every tier is kernel-only; per_tier leaves first-party on
// the normal network.
func TestNewAgentSandbox_KernelOnlyAll(t *testing.T) {
	sb, _ := testAgentSandbox(t, func(o *AgentSandboxOptions) { o.KernelOnlyAll = true })
	p := sb.Policy()
	if !p.Tiers[sandbox.TierFirstParty].DenyNetwork || !p.Tiers[sandbox.TierThirdParty].DenyNetwork {
		t.Fatalf("kernel_only_all tiers: %+v", p.Tiers)
	}
	if got := sb.KernelOnlyTiers(); len(got) != 2 {
		t.Fatalf("kernel-only tiers = %v", got)
	}
	if want := runtime.GOOS != "windows"; sb.kernelOnly(sandbox.TierFirstParty) != want {
		t.Fatalf("first-party kernelOnly = %v on %s", !want, runtime.GOOS)
	}
}

// The kernel-only command: only the --substrate-addr value moves to the
// kernel socket; over TLS the unix authority's server name replaces the TCP one
// (and is dropped when the certificate names localhost).
func TestKernelOnlyCommand(t *testing.T) {
	im := withPython(NewInstanceManager("127.0.0.1:50051"), "python")
	c := sandbox.Command{Path: "python", Args: []string{"a.py", "--socket", "/s/a.sock", "--substrate-addr", "127.0.0.1:50051", "--profile", "x"},
		Env: []string{"PATH=/bin", TLSCAEnv + "=/ca.pem", TLSServerNameEnv + "=kernel.lan"}}
	got := im.kernelOnlyCommand(c, "/s/kernel.sock")
	if want := []string{"a.py", "--socket", "/s/a.sock", "--substrate-addr", "unix:/s/kernel.sock", "--profile", "x"}; !slices.Equal(got.Args, want) {
		t.Fatalf("args = %v", got.Args)
	}
	if c.Args[4] != "127.0.0.1:50051" {
		t.Fatal("the caller's argv was mutated")
	}
	if !slices.Equal(got.Env, c.Env) {
		t.Fatalf("no TLS root on the manager: env must be unchanged, got %v", got.Env)
	}
	im.SetAgentTransport(AgentTransport{TLSCAFile: "/ca.pem", TLSServerName: "kernel.lan", UnixTLSServerName: "kernel.example"})
	got = im.kernelOnlyCommand(c, "/s/kernel.sock")
	if !slices.Contains(got.Env, TLSServerNameEnv+"=kernel.example") || slices.Contains(got.Env, TLSServerNameEnv+"=kernel.lan") ||
		!slices.Contains(got.Forward, TLSServerNameEnv) {
		t.Fatalf("TLS env = %v forward = %v", got.Env, got.Forward)
	}
	im.SetAgentTransport(AgentTransport{TLSCAFile: "/ca.pem", TLSServerName: "kernel.lan"})
	got = im.kernelOnlyCommand(c, "/s/kernel.sock")
	if slices.ContainsFunc(got.Env, func(kv string) bool { return strings.HasPrefix(kv, TLSServerNameEnv+"=") }) {
		t.Fatalf("certificate names localhost: no override expected, env = %v", got.Env)
	}
}

// A kernel-only instance without a kernel socket opener is not started, and
// leaves no socket directory behind.
func TestStartSandboxed_KernelOnlyNeedsTheKernelSocket(t *testing.T) {
	// Windows (ADR-0157 S5): the AppContainer has no loopback, so every tier
	// there needs the kernel socket too.
	sb, _ := testAgentSandbox(t, func(o *AgentSandboxOptions) { o.KernelOnlyAll = true })
	im := withPython(NewInstanceManager("127.0.0.1:1"), "/bin/sh")
	im.SetAgentContainment(AgentContainment{Sandbox: sb})
	def := &domain.AgentDefinition{ID: "probe", Runtime: domain.RuntimeBinary, ExecPath: "/bin/true"}
	inst := domain.NewInstance(def.ID)
	cmd, err := im.buildAgentCmd(def, inst, "")
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, _, err = im.startSandboxed(context.Background(), def, inst, cmd, 0)
	if err == nil || !strings.Contains(err.Error(), "kernel-only") {
		t.Fatalf("err = %v", err)
	}
	if _, serr := os.Stat(sb.socketDir(inst.ID)); !os.IsNotExist(serr) {
		t.Fatalf("socket directory left behind: %v", serr)
	}
}

// The boot log names enforced and missing controls once per agent: INFO when
// all enforced, WARN when degraded; later spawns log at DEBUG.
func TestReportContainment_OncePerAgent(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	im := withPython(NewInstanceManager(""), "python")
	def := &domain.AgentDefinition{ID: "chat_agent"}
	degraded := sandbox.Report{ServerID: "chat_agent-1", Tier: sandbox.TierFirstParty, Runner: "native",
		Enforced: []sandbox.Control{sandbox.ControlEnv, sandbox.ControlFSConfine},
		Missing:  []string{"hide_state: Windows low integrity confines writes only: reads ... are NOT confined"}}
	im.reportContainment(context.Background(), def, degraded, nil)
	im.reportContainment(context.Background(), def, degraded, nil)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "level=WARN") || !strings.Contains(lines[0], "DEGRADED") ||
		!strings.Contains(lines[0], "NOT confined") || !strings.Contains(lines[0], "fs_confine") || !strings.Contains(lines[1], "level=DEBUG") {
		t.Fatalf("log:\n%s", buf.String())
	}
	buf.Reset()
	im.reportContainment(context.Background(), &domain.AgentDefinition{ID: "retrieval"},
		sandbox.Report{Enforced: []sandbox.Control{sandbox.ControlHideState}}, nil)
	if !strings.Contains(buf.String(), "level=INFO") || !strings.Contains(buf.String(), "enforced") {
		t.Fatalf("log:\n%s", buf.String())
	}
}

// ADR-0157 S7: the shared model cache is shown to every instance at its own
// path — writable for system and first-party agents, read-only for
// third-party — private to the agent (the AppContainer grants it to the
// agent's capability), and not shown at all when the kernel resolved none.
func TestAgentBinds_ModelCacheByTier(t *testing.T) {
	root := t.TempDir()
	models := filepath.Join(root, "kernel", "runtime", "models")
	sb, _ := testAgentSandbox(t, func(o *AgentSandboxOptions) { o.ModelCache = models })
	im := withPython(NewInstanceManager("127.0.0.1:1"), "python")
	im.SetAgentContainment(AgentContainment{Sandbox: sb})
	for _, tc := range []struct {
		def   *domain.AgentDefinition
		write bool
	}{
		{&domain.AgentDefinition{ID: "docling_agent", Runtime: domain.RuntimePython, Dir: "agents", System: true}, true},
		{&domain.AgentDefinition{ID: "researcher", Runtime: domain.RuntimePython, Dir: "agents", TrustTier: "first-party"}, true},
		{&domain.AgentDefinition{ID: "vendor", Runtime: domain.RuntimePython, Dir: "agents", TrustTier: "third-party"}, false},
	} {
		binds, err := im.agentBinds(context.Background(), tc.def, "python")
		if err != nil {
			t.Fatal(err)
		}
		want := sandbox.Bind{Path: models, Write: tc.write, Private: true}
		if !slices.Contains(binds, want) {
			t.Errorf("%s: binds %+v lack %+v", tc.def.ID, binds, want)
		}
	}
	none, _ := testAgentSandbox(t, nil)
	im.SetAgentContainment(AgentContainment{Sandbox: none})
	binds, err := im.agentBinds(context.Background(), &domain.AgentDefinition{ID: "x", Runtime: domain.RuntimePython, Dir: "agents"}, "python")
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range binds {
		if strings.Contains(b.Path, filepath.Join("runtime", "models")) {
			t.Errorf("no model cache resolved, yet %s is bound", b.Path)
		}
	}
}
