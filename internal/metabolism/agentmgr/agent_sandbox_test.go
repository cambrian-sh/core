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
	im := NewInstanceManager("/opt/venv/bin/python", "127.0.0.1:50051")
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
	plain := NewInstanceManager("/opt/venv/bin/python", "127.0.0.1:50051")
	boxed := NewInstanceManager("/opt/venv/bin/python", "127.0.0.1:50051")
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
	im := NewInstanceManager(py, "127.0.0.1:1")
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
		spec, c, err := im.sandboxLaunch(context.Background(), tc.def, inst, cmd, 1536)
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
		if spec.Agent.Binds[len(spec.Agent.Binds)-1] != (sandbox.Bind{Path: cache, Write: true}) {
			t.Errorf("%s: the declared cache is not writable: %+v", tc.def.ID, spec.Agent.Binds)
		}
		if !slices.Equal(c.Args, cmd.Args[1:]) || !slices.Equal(c.Env, cmd.Env) || c.Dir != absDir {
			t.Errorf("%s: command changed: %+v", tc.def.ID, c)
		}
		if slices.Contains(c.Forward, "PATH") {
			t.Errorf("%s: a host OS variable is forwarded: %v", tc.def.ID, c.Forward)
		}
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
	if !tp.ConfineFS || !tp.HideState || !tp.LowPrivilege || tp.DenyNetwork || tp.OnUnavailable != sandbox.OnUnavailableDegrade {
		t.Errorf("third-party tier: %+v", tp)
	}
	if def := NewAgentSandbox(AgentSandboxOptions{ThirdPartyRefuse: true}).Policy(); def.Tiers[sandbox.TierFirstParty].OnUnavailable != sandbox.OnUnavailableDegrade || def.WorkRoot == "" {
		t.Errorf("defaults: first-party %q (Q2: degrade), work root %q", def.Tiers[sandbox.TierFirstParty].OnUnavailable, def.WorkRoot)
	}
}

// A third-party agent with the sandbox on is still refused by default: its
// kernel-only network is S2. The message says so.
func TestAdmitLaunch_ThirdPartyWithSandboxNamesTheS2Network(t *testing.T) {
	sb, _ := testAgentSandbox(t, nil)
	im := NewInstanceManager("python", "")
	im.SetAgentContainment(AgentContainment{Sandbox: sb})
	_, err := im.admitLaunch(context.Background(), &domain.AgentDefinition{ID: "vendor", TrustTier: "third-party"})
	if err == nil || !strings.Contains(err.Error(), "ADR-0157 S2") || strings.Contains(err.Error(), "fs_confine") {
		t.Fatalf("err = %v", err)
	}
}

// The boot log names enforced and missing controls once per agent: INFO when
// all enforced, WARN when degraded; later spawns log at DEBUG.
func TestReportContainment_OncePerAgent(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	im := NewInstanceManager("python", "")
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
