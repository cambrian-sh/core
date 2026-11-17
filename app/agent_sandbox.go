package app

import (
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/cambrian-sh/core/internal/backup"
	"github.com/cambrian-sh/core/internal/config"
)

// anchorAgentSandboxPaths resolves execution.agents.sandbox's paths at boot
// (ADR-0157 D1): its relative work_root against the config directory, and the
// kernel-state paths every confined agent is denied, from the blocks that own
// them. The list is computed here, at the composition root, because only here
// are all of them known; it is never read from a config layer, and an
// operator's hide_paths only ADD to it.
//
// Hidden from every agent (D1.1):
//   - the kernel directory (baseDir): its configs/, .env, logs and any loose
//     secrets beside the binary; the agents' code, prompts/ and skills/ inside
//     it are shown again read-only (RuntimeReadPaths);
//   - the config directory: config.db and secret.key (ADR-0101);
//   - the data directory: the bbolt registry, content store, step cache, the
//     reactive journal and the artifact vault;
//   - backups and the encrypted key copies (ENG-681);
//   - the MCP servers' sandbox work root and the server TLS key and card
//     signing key files;
//   - the agent work root (every other instance's scratch and socket) — added
//     by agentmgr.NewAgentSandbox.
func anchorAgentSandboxPaths(cfg *config.Config, configPath, baseDir string) {
	sb := &cfg.Execution.Agents.Sandbox
	confDir := filepath.Dir(configPath)
	if sb.WorkRoot != "" && !filepath.IsAbs(sb.WorkRoot) {
		sb.WorkRoot = filepath.Join(confDir, sb.WorkRoot)
	}
	for i, p := range sb.HidePaths {
		if !filepath.IsAbs(p) {
			sb.HidePaths[i] = filepath.Join(confDir, p)
		}
	}
	sb.KernelDir = absOr(baseDir)
	state := []string{confDir, cfg.Storage.DataDir, cfg.MCP.Sandbox.WorkRoot,
		cfg.Server.TLSKeyFile, cfg.Server.A2A.SigningKeyFile}
	if cfg.Backup.Dir != "" {
		if paths, err := backup.ResolvePaths(cfg.Backup, filepath.Dir(confDir)); err == nil {
			state = append(state, paths.Root, paths.KeyRoot)
		} else {
			slog.Warn("agent sandbox: backup paths not hidden (invalid backup block)", "err", err)
		}
	}
	sb.KernelStatePaths = nil
	for _, p := range state {
		if p = strings.TrimSpace(p); p != "" {
			sb.KernelStatePaths = append(sb.KernelStatePaths, absOr(p))
		}
	}
	// What every agent may read back inside the hidden kernel directory: the
	// agents tree (code and profiles), prompts/ and skills/ — the shipped
	// agents find them by walking up from their own file
	// (agents/worker_agent.py, agents/chat_agent.py).
	sb.RuntimeReadPaths = nil
	for _, p := range []string{cfg.Metabolism.AgentsDir, filepath.Join(baseDir, "prompts"), filepath.Join(baseDir, "skills")} {
		if strings.TrimSpace(p) != "" {
			sb.RuntimeReadPaths = append(sb.RuntimeReadPaths, absOr(p))
		}
	}
}

func absOr(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}
