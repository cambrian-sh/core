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
