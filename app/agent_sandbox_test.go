package app

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
)

// ADR-0157 D1: the composition root resolves the kernel-state paths every
// confined agent is denied; an operator's hide_paths only add to them.
func TestAnchorAgentSandboxPaths(t *testing.T) {
	base := filepath.Join(t.TempDir(), "cambrian")
	cfg := &config.Config{Backup: config.DefaultBackupConfig()}
	cfg.Storage.DataDir = filepath.Join(base, "data")
	cfg.Metabolism.AgentsDir = filepath.Join(base, "agents")
	cfg.MCP.Sandbox.WorkRoot = filepath.Join(base, "mcp-work")
	cfg.Server.TLSKeyFile = filepath.Join(base, "configs", "tls.key")
	sb := &cfg.Execution.Agents.Sandbox
	sb.WorkRoot = "agent-work"
	sb.HidePaths = []string{"pgdata"}
	configPath := filepath.Join(base, "configs", "config.json")

	anchorAgentSandboxPaths(cfg, configPath, base)

	confDir := filepath.Join(base, "configs")
	if sb.WorkRoot != filepath.Join(confDir, "agent-work") || sb.HidePaths[0] != filepath.Join(confDir, "pgdata") {
		t.Fatalf("relative paths not anchored: work_root=%s hide=%v", sb.WorkRoot, sb.HidePaths)
	}
	if sb.KernelDir != base {
		t.Fatalf("kernel dir = %s", sb.KernelDir)
	}
	for _, want := range []string{confDir, cfg.Storage.DataDir, cfg.MCP.Sandbox.WorkRoot, cfg.Server.TLSKeyFile} {
		if !slices.Contains(sb.KernelStatePaths, want) {
			t.Errorf("kernel state %v lacks %s", sb.KernelStatePaths, want)
		}
	}
	if !slices.ContainsFunc(sb.KernelStatePaths, func(p string) bool { return strings.HasSuffix(p, "backups") }) {
		t.Errorf("backups not hidden: %v", sb.KernelStatePaths)
	}
	for _, want := range []string{cfg.Metabolism.AgentsDir, filepath.Join(base, "prompts"), filepath.Join(base, "skills")} {
		if !slices.Contains(sb.RuntimeReadPaths, want) {
			t.Errorf("runtime read paths %v lack %s", sb.RuntimeReadPaths, want)
		}
	}
}

// ADR-0157 D7.1.3: the managed env root is the kernel's; under the kernel dir
// (the default) that hide covers it, a root elsewhere is hidden explicitly so
// bubblewrap's read-only / never shows another agent's package layer.
func TestAnchorAgentSandboxPaths_HidesAnOutsidePythonRoot(t *testing.T) {
	base := filepath.Join(t.TempDir(), "cambrian")
	configPath := filepath.Join(base, "configs", "config.json")
	cfg := &config.Config{Backup: config.DefaultBackupConfig()}
	anchorAgentSandboxPaths(cfg, configPath, base)
	defRoot := filepath.Join(base, "runtime", "python")
	if slices.Contains(cfg.Execution.Agents.Sandbox.KernelStatePaths, defRoot) {
		t.Errorf("the default root lies in the hidden kernel dir; it needs no own entry: %v", cfg.Execution.Agents.Sandbox.KernelStatePaths)
	}
	outside := filepath.Join(t.TempDir(), "pyenv")
	cfg = &config.Config{Backup: config.DefaultBackupConfig()}
	cfg.Execution.Agents.Python.Root = outside
	anchorAgentSandboxPaths(cfg, configPath, base)
	if !slices.Contains(cfg.Execution.Agents.Sandbox.KernelStatePaths, outside) {
		t.Fatalf("an env root outside the kernel dir must be hidden: %v", cfg.Execution.Agents.Sandbox.KernelStatePaths)
	}
}
