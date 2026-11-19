package app

import (
	"errors"
	"log/slog"
	"path/filepath"

	"github.com/cambrian-sh/core/internal/backup"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/integrations"
	"github.com/cambrian-sh/core/internal/sandbox"
	"github.com/cambrian-sh/core/internal/sandbox/egressproxy"
)

// Trust-tier containment wiring (ADR-0139 D8,
// T-20261105-stdio-mcp-servers-run-with-the-kernel-user-s): config mcp.sandbox
// plus the integration catalog become ONE *sandbox.Policy at boot, and every
// mcp.ServerConfig the kernel builds (boot, runtime save, console probe,
// plugin servers, register_mcp) carries a Spec resolved from it.

// anchorSandboxPaths resolves the sandbox's relative paths against the config
// bundle, like the other boot paths, and defaults hide_paths to the kernel's
// config and data directories (secret.key, config.db, the bbolt store) and the backup
// directories.
func anchorSandboxPaths(cfg *config.Config, configPath string) {
	dir := filepath.Dir(configPath)
	sb := &cfg.MCP.Sandbox
	if sb.WorkRoot != "" && !filepath.IsAbs(sb.WorkRoot) {
		sb.WorkRoot = filepath.Join(dir, sb.WorkRoot)
	}
	if sb.HidePaths == nil {
		sb.HidePaths = []string{dir}
		if cfg.Storage.DataDir != "" {
			sb.HidePaths = append(sb.HidePaths, cfg.Storage.DataDir)
		}
		// ENG-681: database dumps and the encrypted secret.key copies are as sensitive
		// as the store itself.
		if cfg.Backup.Dir != "" {
			if paths, err := backup.ResolvePaths(cfg.Backup, filepath.Dir(dir)); err == nil {
				sb.HidePaths = append(sb.HidePaths, paths.Root, paths.KeyRoot)
			} else {
				slog.Warn("mcp sandbox: backup paths not hidden (invalid backup block)", "err", err)
			}
		}
	}
	cfg.MCP.CatalogFile = integrations.ResolvePath(cfg.MCP.CatalogFile, configPath)
}

// sandboxPolicyFromConfig overlays mcp.sandbox on the built-in defaults and
// attaches the catalog's per-entry tier and sandbox declaration. egress is the
// kernel's started egress allowlist proxy (ENG-688); nil leaves declared hosts
// recorded, not filtered (the reports say so).
func sandboxPolicyFromConfig(c config.MCPSandboxConfig, cat *integrations.Catalog, egress *egressproxy.Broker) *sandbox.Policy {
	p := sandbox.DefaultPolicy()
	p.Egress = egress
	p.Disabled = !c.SandboxEnabled()
	if t, ok := sandbox.ParseTier(c.DefaultTier); ok {
		p.DefaultTier = t
	}
	for k, v := range c.CatalogTiers {
		if t, ok := sandbox.ParseTier(v); ok {
			p.CatalogTiers[k] = t
		}
	}
	if c.WorkRoot != "" {
		p.WorkRoot = c.WorkRoot
	}
	p.BwrapPath, p.DockerPath, p.CgroupParent = c.BwrapPath, c.DockerPath, c.CgroupParent
	for k, v := range c.DockerImages {
		p.DockerImages[k] = v
	}
	p.HidePaths = append([]string(nil), c.HidePaths...)
	for name, o := range c.Tiers {
		t, ok := sandbox.ParseTier(name)
		if !ok {
			continue // refused by config validation
		}
		tp := p.Tiers[t]
		if o.OnUnavailable != "" {
			tp.OnUnavailable = o.OnUnavailable
		}
		if o.Runner != "" {
			tp.Runner = o.Runner
		}
		setBool(&tp.ConfineFS, o.ConfineFS)
		setBool(&tp.DenyNetwork, o.DenyNetwork)
		setBool(&tp.LowPrivilege, o.LowPrivilege)
		setInt(&tp.MemoryMB, o.MemoryMB)
		setInt(&tp.MaxProcesses, o.MaxProcesses)
		setInt(&tp.CPUTimeSec, o.CPUTimeSec)
		setInt(&tp.StallTimeoutSec, o.StallTimeoutSec)
		setInt(&tp.MaxMessageKB, o.MaxMessageKB)
		p.Tiers[t] = tp
	}
	p.Catalog = map[string]sandbox.CatalogEntry{}
	for _, e := range cat.Entries() {
		p.Catalog[e.ID] = sandbox.CatalogEntry{TrustTier: e.TrustTier, Server: serverSandboxSpec(e.Server.Sandbox)}
	}
	if p.Disabled {
		slog.Warn("ADR-0139 D8: mcp.sandbox.enabled=false — stdio MCP servers get the env allowlist only, no process containment")
	}
	return p
}

// loadSandboxCatalog loads the integration catalog for tier resolution, with
// the same fallback the console uses: a deployment file whose entries are
// invalid leaves the built-in catalog.
//
// A deployment file that does not parse at all is a boot error instead
// (config.ErrMalformedConfigFile, owner decision 2026-11-18, ENG-2121): a config
// file the operator wrote is never silently dropped, and the parse fault is
// almost always an editor accident (a trailing comma) the operator fixes in
// seconds once the error names the file and line.
func loadSandboxCatalog(path string) (*integrations.Catalog, error) {
	cat, err := integrations.Load(path)
	if errors.Is(err, config.ErrMalformedConfigFile) {
		return nil, err
	}
	if err != nil {
		slog.Error("ADR-0139 D8: integration catalog invalid; sandbox tiers use the built-in catalog only", "file", path, "err", err)
		if cat, err = integrations.Load(""); err != nil {
			slog.Error("ADR-0139 D8: built-in integration catalog failed to load; catalog servers launch at the default tier", "err", err)
			return nil, nil
		}
	}
	return cat, nil
}

// serverSandboxSpec maps a server's sandbox declaration.
func serverSandboxSpec(c *config.MCPServerSandboxConfig) sandbox.ServerSpec {
	if c == nil {
		return sandbox.ServerSpec{}
	}
	return sandbox.ServerSpec{
		Network: c.Network, Hosts: append([]string(nil), c.Hosts...), Root: c.Root,
		Runner: c.Runner, Image: c.Image, MemoryMB: c.MemoryMB, MaxProcesses: c.MaxProcesses,
	}
}

// containmentSpec resolves a configured server's containment.
func containmentSpec(s config.MCPServerConfig, sb *sandbox.Policy) sandbox.Spec {
	return sb.Resolve(s.ID, s.TrustTier, s.CatalogID, false, serverSandboxSpec(s.Sandbox))
}

func setBool(dst *bool, v *bool) {
	if v != nil {
		*dst = *v
	}
}

func setInt(dst *int, v *int) {
	if v != nil {
		*dst = *v
	}
}
