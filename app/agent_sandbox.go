package app

import (
	"log/slog"
	"os"
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
	// ADR-0157 D7.1.3: the managed Python env root (downloads/, cache/,
	// current.json and every agent's package layer) is the kernel's; an agent is
	// shown only its base generation and its own layer, as binds. Under the
	// kernel directory (the default) that hide already covers it; an operator
	// root elsewhere is hidden explicitly, so bubblewrap's read-only / never
	// shows another agent's layer.
	if root := absOr(cfg.Execution.Agents.Python.ResolvedRoot(absOr(baseDir))); !pathWithin(root, sb.KernelDir) {
		state = append(state, root)
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
	cfg.Execution.Agents.ModelCache = anchorModelCache(cfg.Execution.Agents, absOr(baseDir), sb.KernelStatePaths)
}

// anchorModelCache resolves and creates the agents' shared model cache
// (ADR-0157 S7, config.AgentsConfig.ModelCacheDir). It may sit inside the
// hidden kernel directory (the default, runtime/models, is shown again as a
// bind like the managed Python env) but never inside the kernel's own state:
// config.db, the data directory or a backup must not become agent-writable
// because an operator pointed the cache there. An unusable directory is named
// at ERROR and no agent is pointed at a cache — they download into their
// scratch, slower but never into kernel state.
func anchorModelCache(ag config.AgentsConfig, baseDir string, kernelState []string) string {
	dir := absOr(ag.ResolvedModelCacheDir(baseDir))
	for _, st := range kernelState {
		if pathWithin(dir, st) || pathWithin(st, dir) {
			slog.Error("execution.agents.model_cache_dir overlaps the kernel's own state; agents get no shared model cache (ADR-0157 S7)",
				"model_cache_dir", dir, "kernel_state", st)
			return ""
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Error("execution.agents.model_cache_dir cannot be created; agents get no shared model cache (ADR-0157 S7)",
			"model_cache_dir", dir, "err", err)
		return ""
	}
	return dir
}

// pathWithin reports whether p is dir or lies under it.
func pathWithin(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func absOr(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}
