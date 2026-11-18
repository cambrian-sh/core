package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/pyenv"
)

// The one builder (ADR-0157 D7.11.1) takes the embedded pin table and the
// embedded hashed base lock; its root defaults under the kernel base dir.
func TestNewPythonEnvBuilderUsesEmbeddedPinsAndLock(t *testing.T) {
	base := t.TempDir()
	b, err := newPythonEnvBuilder(config.DefaultConfig().Execution.Agents.Python, base, filepath.Join(base, "configs"))
	if err != nil {
		t.Skipf("no pinned build for this host: %v", err)
	}
	if b.Root != filepath.Join(base, "runtime", "python") {
		t.Fatalf("root %s", b.Root)
	}
	in, lock, err := b.Inputs()
	if err != nil {
		t.Fatalf("embedded inputs: %v", err)
	}
	if !lock.Has(pyenv.SDKProject) || in.Python.SHA256 == "" || in.Installer.SHA256 == "" {
		t.Fatalf("inputs %+v", in)
	}
	pc := config.AgentPythonConfig{Offline: true, Wheelhouse: []string{"wheels"}, PythonMirrorURL: "file:///srv/m/"}
	b, err = newPythonEnvBuilder(pc, base, filepath.Join(base, "configs"))
	if err != nil {
		t.Fatal(err)
	}
	if !b.Sources.Offline || b.Sources.Wheelhouse[0] != filepath.Join(base, "configs", "wheels") || b.Sources.PythonMirror != "file:///srv/m/" {
		t.Fatalf("sources %+v", b.Sources)
	}
}

// With build_on_boot off and nothing built, the boot gate refuses Python
// agents and names the fix; the kernel itself is not blocked.
func TestStartPythonEnvRefusesWhenMissingAndBuildOff(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Execution.Agents.Python.BuildOnBoot = false
	gate := startPythonEnv(context.Background(), cfg, t.TempDir())
	defer gate.Stop()
	err := gate.Ready()
	if err == nil || !strings.Contains(err.Error(), pyenv.FixCommand) {
		t.Fatalf("want a refusal naming %q, got %v", pyenv.FixCommand, err)
	}
}

func TestPythonEnvCLI(t *testing.T) {
	dir := t.TempDir()
	var out, errb bytes.Buffer
	if code := runPythonEnv(context.Background(), nil, &out, &errb); code != 2 {
		t.Fatalf("no verb: %d", code)
	}
	out.Reset()
	if code := runPythonEnv(context.Background(), []string{"status", "--dir", dir}, &out, &errb); code != 1 {
		t.Fatalf("status of an unbuilt env must fail: %d %s %s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "missing") || !strings.Contains(out.String(), pyenv.FixCommand) {
		t.Fatalf("status output: %s", out.String())
	}
	// An invalid block is refused before anything is fetched.
	if err := os.MkdirAll(filepath.Join(dir, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "configs", "config.json"), []byte(`{"execution":{"agents":{"python":{"offline":true}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	errb.Reset()
	if code := runPythonEnv(context.Background(), []string{"build", "--dir", dir}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "wheelhouse") {
		t.Fatalf("invalid block: %d %s", code, errb.String())
	}
}
