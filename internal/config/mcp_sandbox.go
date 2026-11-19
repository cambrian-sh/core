package config

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// MCPSandboxConfig is mcp.sandbox: trust-tier containment for every stdio MCP
// server the kernel launches (ADR-0139 D8, T-20261105-stdio-mcp-servers-run-
// with-the-kernel-user-s). Every field is optional: an unset field keeps the
// built-in default in internal/sandbox.DefaultPolicy (the defaults are listed
// in the flags catalog and configs/MCP.md). Threaded to the consumer at the
// composition root (app.sandboxPolicyFromConfig → every mcp.ServerConfig).
type MCPSandboxConfig struct {
	// Enabled nil ⇒ true. false launches every server with the env allowlist
	// only and logs a WARN per launch.
	Enabled *bool `json:"enabled,omitempty"`
	// DefaultTier is the tier of a server that declares none and has no
	// catalog entry (a custom or model-registered server). Default third-party.
	DefaultTier string `json:"default_tier,omitempty"`
	// CatalogTiers maps the integration catalog's trust_tier vocabulary to a
	// launch tier. Default {official: vetted, curated: vetted, community: third-party}; given
	// keys replace those defaults one by one.
	CatalogTiers map[string]string `json:"catalog_tiers,omitempty"`
	// WorkRoot holds each server's working directory (<work_root>/<tier>/<id>).
	// Default <user cache dir>/cambrian/sandbox. Relative paths resolve against
	// the primary config file's directory. On a root Linux kernel it must be
	// traversable by other users (not under /root) for the uid drop.
	WorkRoot string `json:"work_root,omitempty"`
	// BwrapPath / DockerPath locate bubblewrap and the docker CLI ("" ⇒ PATH).
	BwrapPath  string `json:"bwrap_path,omitempty"`
	DockerPath string `json:"docker_path,omitempty"`
	// DockerImages maps a launch command's basename (npx, uvx, uv, node,
	// python) to the image the docker runner uses; given keys replace defaults.
	DockerImages map[string]string `json:"docker_images,omitempty"`
	// HidePaths are masked from a bubblewrap-confined server. Default: the
	// directory of the primary config file (secret.key, config.db).
	HidePaths []string `json:"hide_paths,omitempty"`
	// CgroupParent is a delegated cgroup v2 directory (Linux) for per-server
	// memory.max / pids.max. "" ⇒ none.
	CgroupParent string `json:"cgroup_parent,omitempty"`
	// Tiers overrides one tier's policy (keys first-party | vetted | third-party).
	Tiers map[string]MCPSandboxTierConfig `json:"tiers,omitempty"`
}

// MCPSandboxTierConfig overrides one tier's containment policy. nil fields
// keep the default; a limit of 0 switches that one limit off.
type MCPSandboxTierConfig struct {
	// OnUnavailable: "refuse" (fail closed) or "degrade" (start and report)
	// when a wanted control cannot be enforced on this platform.
	OnUnavailable string `json:"on_unavailable,omitempty"`
	// Runner: "native" or "docker".
	Runner          string `json:"runner,omitempty"`
	ConfineFS       *bool  `json:"confine_fs,omitempty"`
	DenyNetwork     *bool  `json:"deny_network,omitempty"`
	LowPrivilege    *bool  `json:"low_privilege,omitempty"`
	MemoryMB        *int   `json:"memory_mb,omitempty"`
	MaxProcesses    *int   `json:"max_processes,omitempty"`
	CPUTimeSec      *int   `json:"cpu_time_sec,omitempty"`
	StallTimeoutSec *int   `json:"stall_timeout_sec,omitempty"`
	MaxMessageKB    *int   `json:"max_message_kb,omitempty"`
}

// MCPServerSandboxConfig is one stdio server's own containment declaration
// (mcp.servers[].sandbox, or a catalog entry's server.sandbox).
type MCPServerSandboxConfig struct {
	// Network: "none" (default) or "egress". A third-party server that
	// declares nothing gets no network where the platform can deny it.
	Network string `json:"network,omitempty"`
	// Hosts the server needs (egress only): host, host:port (no port ⇒ 443),
	// *.domain, or an IP address. Where the tier denies the network and the
	// runner can (docker, bubblewrap), the server reaches ONLY these hosts,
	// through the kernel's CONNECT proxy (ENG-688); "*" or free text leaves
	// the network unfiltered, and the containment report says so.
	Hosts []string `json:"hosts,omitempty"`
	// Root is an extra writable directory for a confined server (absolute).
	Root string `json:"root,omitempty"`
	// Runner overrides the tier's runner ("native" | "docker"); Image picks
	// the docker image.
	Runner string `json:"runner,omitempty"`
	Image  string `json:"image,omitempty"`
	// MemoryMB / MaxProcesses override the tier's limits for this server
	// (a headless browser needs more than a calculator).
	MemoryMB     int `json:"memory_mb,omitempty"`
	MaxProcesses int `json:"max_processes,omitempty"`
}

// SandboxEnabled reports whether trust-tier containment is on (default true).
func (c MCPSandboxConfig) SandboxEnabled() bool { return c.Enabled == nil || *c.Enabled }

var (
	validSandboxTiers   = []string{"first-party", "vetted", "third-party"}
	validSandboxRunners = []string{"native", "docker"}
)

func oneOf(v string, set []string) bool {
	for _, s := range set {
		if v == s {
			return true
		}
	}
	return false
}

// validate checks mcp.sandbox.
func (c MCPSandboxConfig) validate() []string {
	var errs []string
	if c.DefaultTier != "" && !oneOf(c.DefaultTier, validSandboxTiers) {
		errs = append(errs, fmt.Sprintf("mcp.sandbox.default_tier %q must be one of %s", c.DefaultTier, strings.Join(validSandboxTiers, ", ")))
	}
	keys := make([]string, 0, len(c.CatalogTiers))
	for k := range c.CatalogTiers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v := c.CatalogTiers[k]; !oneOf(v, validSandboxTiers) {
			errs = append(errs, fmt.Sprintf("mcp.sandbox.catalog_tiers.%s %q must be one of %s", k, v, strings.Join(validSandboxTiers, ", ")))
		}
	}
	tiers := make([]string, 0, len(c.Tiers))
	for k := range c.Tiers {
		tiers = append(tiers, k)
	}
	sort.Strings(tiers)
	for _, k := range tiers {
		t := c.Tiers[k]
		p := "mcp.sandbox.tiers." + k
		if !oneOf(k, validSandboxTiers) {
			errs = append(errs, fmt.Sprintf("%s: unknown tier (want %s)", p, strings.Join(validSandboxTiers, ", ")))
		}
		if t.OnUnavailable != "" && t.OnUnavailable != "refuse" && t.OnUnavailable != "degrade" {
			errs = append(errs, fmt.Sprintf("%s.on_unavailable %q must be refuse or degrade", p, t.OnUnavailable))
		}
		if t.Runner != "" && !oneOf(t.Runner, validSandboxRunners) {
			errs = append(errs, fmt.Sprintf("%s.runner %q must be native or docker", p, t.Runner))
		}
		for name, v := range map[string]*int{"memory_mb": t.MemoryMB, "max_processes": t.MaxProcesses,
			"cpu_time_sec": t.CPUTimeSec, "stall_timeout_sec": t.StallTimeoutSec, "max_message_kb": t.MaxMessageKB} {
			if v != nil && *v < 0 {
				errs = append(errs, fmt.Sprintf("%s.%s must be >= 0 (0 switches the limit off)", p, name))
			}
		}
	}
	if c.CgroupParent != "" && !filepath.IsAbs(c.CgroupParent) {
		errs = append(errs, "mcp.sandbox.cgroup_parent must be an absolute path")
	}
	sort.Strings(errs)
	return errs
}

// ValidateMCPServerSandbox checks one server's trust_tier and sandbox block.
// Shared by config load, console writes (ValidateMCPServer) and the
// integration catalog (whose entries declare the server's sandbox needs).
func ValidateMCPServerSandbox(p string, tier string, sb *MCPServerSandboxConfig) []string {
	var errs []string
	if tier != "" && !oneOf(tier, validSandboxTiers) {
		msg := fmt.Sprintf("%s.trust_tier %q must be one of %s", p, tier, strings.Join(validSandboxTiers, ", "))
		if tier == "system" {
			msg += " (system is the kernel itself; no launched process holds it)"
		}
		errs = append(errs, msg)
	}
	if sb == nil {
		return errs
	}
	switch sb.Network {
	case "", "none", "egress":
	default:
		errs = append(errs, fmt.Sprintf("%s.sandbox.network %q must be none or egress", p, sb.Network))
	}
	if len(sb.Hosts) > 0 && sb.Network != "egress" {
		errs = append(errs, fmt.Sprintf("%s.sandbox.hosts needs network: egress", p))
	}
	if sb.Root != "" && !filepath.IsAbs(sb.Root) {
		errs = append(errs, fmt.Sprintf("%s.sandbox.root must be an absolute path", p))
	}
	if sb.Runner != "" && !oneOf(sb.Runner, validSandboxRunners) {
		errs = append(errs, fmt.Sprintf("%s.sandbox.runner %q must be native or docker", p, sb.Runner))
	}
	if sb.MemoryMB < 0 || sb.MaxProcesses < 0 {
		errs = append(errs, fmt.Sprintf("%s.sandbox limits must be >= 0", p))
	}
	return errs
}
