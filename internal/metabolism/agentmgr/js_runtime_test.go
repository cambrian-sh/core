package agentmgr

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ── ADR-0125: bun/node spawn branch ──────────────────────────────────────────

func TestBuildAgentCmd_JSRuntimes(t *testing.T) {
	m := NewAgentManager(nil, "/usr/bin/python3", "localhost:50051", nil)
	m.SetRuntimeExecutable(domain.RuntimeBun, "/opt/bun/bun")
	m.SetRuntimeExecutable(domain.RuntimeNode, "/usr/bin/node")
	inst := domain.NewInstance("js-agent")

	t.Run("bun agent uses the configured interpreter and the shared argv contract", func(t *testing.T) {
		def := &domain.AgentDefinition{
			ID:       "ts_agent",
			Runtime:  domain.RuntimeBun,
			ExecPath: "ts_agent.ts",
			Dir:      "/agents",
		}
		cmd, err := m.buildAgentCmd(def, inst, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cmd.Args[0] != "/opt/bun/bun" {
			t.Errorf("argv[0]: want configured bun, got %q", cmd.Args[0])
		}
		if cmd.Args[1] != "ts_agent.ts" {
			t.Errorf("argv[1]: want exec path, got %q", cmd.Args[1])
		}
		for _, want := range []string{"--socket", "--substrate-addr", "localhost:50051"} {
			if !slices.Contains(cmd.Args, want) {
				t.Errorf("want %q in args, got %v", want, cmd.Args)
			}
		}
	})

	t.Run("node agent uses the configured interpreter", func(t *testing.T) {
		def := &domain.AgentDefinition{
			ID:       "js_agent",
			Runtime:  domain.RuntimeNode,
			ExecPath: "js_agent.js",
			Dir:      "/agents",
		}
		cmd, err := m.buildAgentCmd(def, inst, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cmd.Args[0] != "/usr/bin/node" {
			t.Errorf("argv[0]: want configured node, got %q", cmd.Args[0])
		}
	})

	t.Run("JS env is SEC-01 allowlisted with color suppressed, no PYTHON vars", func(t *testing.T) {
		def := &domain.AgentDefinition{
			ID:       "ts_agent",
			Runtime:  domain.RuntimeBun,
			ExecPath: "ts_agent.ts",
			Dir:      "/agents",
		}
		cmd, err := m.buildAgentCmd(def, inst, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !slices.Contains(cmd.Env, "NO_COLOR=1") || !slices.Contains(cmd.Env, "FORCE_COLOR=0") {
			t.Errorf("want NO_COLOR=1 and FORCE_COLOR=0 in env, got %v", cmd.Env)
		}
		for _, e := range cmd.Env {
			if strings.HasPrefix(e, "PYTHON") {
				t.Errorf("python-only env var leaked into a JS agent: %s", e)
			}
		}
	})

	t.Run("daemon flags apply to JS agents unchanged", func(t *testing.T) {
		def := &domain.AgentDefinition{
			ID:       "js_daemon",
			Runtime:  domain.RuntimeBun,
			ExecPath: "js_daemon.ts",
			Dir:      "/agents",
			Trait:    domain.TraitDaemon,
		}
		cmd, err := m.buildAgentCmd(def, inst, "stream-7")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for _, want := range []string{"--daemon-mode", "--stream-id", "stream-7", "--agent-id", "js_daemon"} {
			if !slices.Contains(cmd.Args, want) {
				t.Errorf("want %q in args, got %v", want, cmd.Args)
			}
		}
	})
}

func TestRuntimeExecutable_UnresolvedNamesConfigKey(t *testing.T) {
	// Empty PATH defeats the $PATH fallback deterministically.
	t.Setenv("PATH", "")
	im := NewInstanceManager("/usr/bin/python3", "localhost:50051")
	_, err := im.runtimeExecutable(domain.RuntimeBun)
	if err == nil {
		t.Fatal("want error when bun is neither configured nor on PATH")
	}
	if !strings.Contains(err.Error(), "metabolism.bun_executable") {
		t.Errorf("error must name the config key, got: %v", err)
	}
}

func TestSetRuntimeExecutable_EmptyClears(t *testing.T) {
	im := NewInstanceManager("/usr/bin/python3", "localhost:50051")
	im.SetRuntimeExecutable(domain.RuntimeNode, "/usr/bin/node")
	im.SetRuntimeExecutable(domain.RuntimeNode, "")
	if p := im.runtimeExes[domain.RuntimeNode]; p != "" {
		t.Errorf("empty path must clear the entry, got %q", p)
	}
}

// ── ADR-0125 D6: JS deps presence check ──────────────────────────────────────

func TestVerifyJSDeps(t *testing.T) {
	im := NewInstanceManager("/usr/bin/python3", "localhost:50051")

	def := func(dir string) *domain.AgentDefinition {
		return &domain.AgentDefinition{
			ID:       "ts_agent",
			Runtime:  domain.RuntimeBun,
			ExecPath: "pkg/agent.ts",
			Dir:      dir,
		}
	}

	t.Run("no package.json ⇒ nothing to verify", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := im.verifyJSDeps(def(root)); err != nil {
			t.Errorf("want nil, got %v", err)
		}
	})

	t.Run("package.json without node_modules ⇒ error naming bun install", func(t *testing.T) {
		root := t.TempDir()
		pkg := filepath.Join(root, "pkg")
		if err := os.MkdirAll(pkg, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{"dependencies":{"zod":"^3"}}`), 0o644); err != nil {
			t.Fatal(err)
		}
		err := im.verifyJSDeps(def(root))
		if err == nil {
			t.Fatal("want error when node_modules is missing")
		}
		if !strings.Contains(err.Error(), "bun install") {
			t.Errorf("error must name the fix, got: %v", err)
		}
	})

	t.Run("node_modules in the unit dir satisfies the check", func(t *testing.T) {
		root := t.TempDir()
		pkg := filepath.Join(root, "pkg")
		if err := os.MkdirAll(filepath.Join(pkg, "node_modules"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := im.verifyJSDeps(def(root)); err != nil {
			t.Errorf("want nil, got %v", err)
		}
	})

	t.Run("hoisted node_modules at the agents root satisfies the check", func(t *testing.T) {
		root := t.TempDir()
		pkg := filepath.Join(root, "pkg")
		if err := os.MkdirAll(pkg, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(root, "node_modules"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := im.verifyJSDeps(def(root)); err != nil {
			t.Errorf("want nil, got %v", err)
		}
	})

	t.Run("non-JS runtimes are exempt", func(t *testing.T) {
		root := t.TempDir()
		d := def(root)
		d.Runtime = domain.RuntimePython
		if err := im.verifyJSDeps(d); err != nil {
			t.Errorf("want nil for python runtime, got %v", err)
		}
	})
}

// ── ADR-0125 D8: Linux RLIMIT_AS exemption for V8 runtimes ───────────────────

func TestSpawnMemLimitMB_JSExemption(t *testing.T) {
	im := NewInstanceManager("/usr/bin/python3", "localhost:50051")
	im.SetAgentMemoryLimitMB(512)

	py := &domain.AgentDefinition{ID: "py_agent", Runtime: domain.RuntimePython}
	if got := im.spawnMemLimitMB(py); got != 512 {
		t.Errorf("python cap: want 512, got %d", got)
	}

	js := &domain.AgentDefinition{ID: "ts_agent", Runtime: domain.RuntimeBun}
	want := 512
	if goruntime.GOOS == "linux" {
		want = 0 // RLIMIT_AS would kill V8's virtual reservations spuriously
	}
	if got := im.spawnMemLimitMB(js); got != want {
		t.Errorf("js cap on %s: want %d, got %d", goruntime.GOOS, want, got)
	}
}
