package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

// AgentSandboxConfig is execution.agents.sandbox: how a kernel-spawned agent
// process is contained (ADR-0139 D8 trust tiers, owner decision 2026-11-14 on
// T-20261106-kernel-spawned-agents-are-not-contained-by; ADR-0157 agent least
// privilege, accepted 2026-11-16). It is the agents' counterpart of mcp.sandbox
// (MCP stdio servers): agents are not MCP servers, so they get their own block
// (AGENTS.md §3.4), with the same tier and on_unavailable vocabulary.
//
// The tier an agent runs at is the kernel's view (agentmgr.agentTrustTier, the
// same rule as the agent card): a composition-root system agent and a local agent
// that declares no tier are first-party; a profile's declared trust_tier is kept.
//
// enabled: false (the default) — the launch is unchanged from before ADR-0157:
// the env allowlist (procenv), the SEC-01 memory cap, a lifetime-bound process,
// the socket under the OS temp directory; a third-party agent is refused unless
// third_party.on_unavailable is degrade (ENG-716 stage 1).
//
// enabled: true — ADR-0157 S1: every agent of every tier launches through
// internal/sandbox (sandbox.StartCmd) with D1 confinement: a private ephemeral
// scratch directory (its TEMP/TMP/TMPDIR/HOME), its socket in a per-instance
// directory under work_root, and the kernel's own state (the kernel directory,
// configs/ with config.db and secret.key, the data directory, backups, the MCP
// and agent work roots, plus hide_paths) unreadable where the runner can do it.
// A control the runner cannot enforce is MISSING and the tier's on_unavailable
// decides; the boot log names enforced and missing controls per agent.
type AgentSandboxConfig struct {
	// Enabled launches agents through the sandbox (ADR-0157 S1). Default false.
	Enabled bool `json:"enabled,omitempty"`
	// Runner: "native" (default; Linux bubblewrap, Windows Job Object + low
	// integrity) or "docker" (Linux hosts: one container per agent instance).
	Runner string `json:"runner,omitempty"`
	// WorkRoot holds every instance's scratch and socket directory; relative
	// paths resolve against the config directory. "" ⇒ <user cache>/cambrian/agents.
	WorkRoot string `json:"work_root,omitempty"`
	// HidePaths are ADDED to the kernel-state paths the kernel always hides
	// (e.g. a local Postgres data directory). They never replace them.
	HidePaths []string `json:"hide_paths,omitempty"`
	// AllowlistCap is the operator's cap on the paths an agent manifest may
	// declare (sandbox_paths): a declared path must lie under one of these
	// absolute paths, else the agent is refused at boot with the path named
	// (ADR-0157 D1.2, Q3). Empty ⇒ no declared path is admitted.
	AllowlistCap []string `json:"allowlist_cap,omitempty"`
	// DockerImage is the image the docker runner starts agents from; it must
	// provide the agent runtime at the kernel's launch path.
	DockerImage string `json:"docker_image,omitempty"`
	// DockerUser is the docker runner's --user ("" ⇒ 65534:65534): a separate
	// non-root identity, never the kernel's (ADR-0157 D2.1).
	DockerUser string `json:"docker_user,omitempty"`
	// Network is the agents' network policy when enabled (ADR-0157 D3, Q3):
	// "per_tier" (default; "" means it) — third-party agents are kernel-only
	// (no network namespace / --network none; they reach the kernel through a
	// unix socket in their socket directory), first-party and system agents keep
	// the normal network; "kernel_only_all" — every agent is kernel-only, for
	// sovereign and defence installs (first-party agents then reach the outside
	// only through kernel-mediated tools).
	Network string `json:"network,omitempty"`
	// FirstParty is the first-party (and system) tier's policy when enabled.
	FirstParty AgentFirstPartySandbox `json:"first_party"`
	ThirdParty AgentTierSandbox       `json:"third_party"`

	// KernelStatePaths and RuntimeReadPaths are resolved at boot by the
	// composition root (app.anchorAgentSandboxPaths) from the other config
	// blocks; they are never read from a config layer.
	//
	// KernelStatePaths: the D1 hide-list (with HidePaths). A manifest-declared
	// path inside one of them is refused, whatever allowlist_cap says.
	KernelStatePaths []string `json:"-"`
	// KernelDir is the kernel's own directory: hidden too, but the agents'
	// code, prompts, skills and runtime inside it are shown again read-only.
	KernelDir string `json:"-"`
	// RuntimeReadPaths are the kernel-known paths every agent may read: the
	// agents directory, prompts/, skills/, the configured interpreters' prefixes.
	RuntimeReadPaths []string `json:"-"`
}

// AgentFirstPartySandbox is the first-party tier's agent containment.
type AgentFirstPartySandbox struct {
	// OnUnavailable: "degrade" (start, WARN once per agent naming what is
	// missing) or "refuse". "" ⇒ degrade: ADR-0157 Q2, owner 2026-11-16 —
	// degrade with a WARN on development platforms that cannot hide reads.
	OnUnavailable string `json:"on_unavailable,omitempty"`
}

// Refuses reports whether a missing control refuses a first-party launch.
func (t AgentFirstPartySandbox) Refuses() bool { return t.OnUnavailable == "refuse" }

// AgentTierSandbox is one tier's agent containment.
type AgentTierSandbox struct {
	// OnUnavailable: "refuse" or "degrade" ("" ⇒ refuse: fail closed).
	OnUnavailable string `json:"on_unavailable,omitempty"`
	// MemoryMB is the tier's memory cap; a smaller per-agent manifest
	// memory_limit_mb still applies, a larger one does not lift it. 0 ⇒ 2048.
	MemoryMB int `json:"memory_mb,omitempty"`
}

// Refuses reports whether a missing control refuses the launch.
func (t AgentTierSandbox) Refuses() bool { return t.OnUnavailable != "degrade" }

// Memory is MemoryMB with its default applied.
func (t AgentTierSandbox) Memory() int { return orDefault(t.MemoryMB, 2048) }

// Agent network policies (execution.agents.sandbox.network).
const (
	AgentNetworkPerTier       = "per_tier"
	AgentNetworkKernelOnlyAll = "kernel_only_all"
)

// KernelOnlyAll reports whether every agent tier is kernel-only (ADR-0157 D3).
func (c AgentSandboxConfig) KernelOnlyAll() bool { return c.Network == AgentNetworkKernelOnlyAll }

// NetworkOrDefault is Network with its default applied.
func (c AgentSandboxConfig) NetworkOrDefault() string {
	if c.Network == "" {
		return AgentNetworkPerTier
	}
	return c.Network
}

// RunnerOrDefault is Runner with its default applied.
func (c AgentSandboxConfig) RunnerOrDefault() string {
	if c.Runner == "" {
		return "native"
	}
	return c.Runner
}

// validate checks execution.agents.sandbox.
func (c AgentSandboxConfig) validate() []string {
	var errs []string
	switch c.ThirdParty.OnUnavailable {
	case "", "refuse", "degrade":
	default:
		errs = append(errs, fmt.Sprintf("execution.agents.sandbox.third_party.on_unavailable must be \"refuse\" or \"degrade\" (got %q)", c.ThirdParty.OnUnavailable))
	}
	switch c.FirstParty.OnUnavailable {
	case "", "refuse", "degrade":
	default:
		errs = append(errs, fmt.Sprintf("execution.agents.sandbox.first_party.on_unavailable must be \"refuse\" or \"degrade\" (got %q)", c.FirstParty.OnUnavailable))
	}
	if c.ThirdParty.MemoryMB < 0 {
		errs = append(errs, "execution.agents.sandbox.third_party.memory_mb must be >= 0 (0 = 2048)")
	}
	switch c.Runner {
	case "", "native", "docker":
	default:
		errs = append(errs, fmt.Sprintf("execution.agents.sandbox.runner must be \"native\" or \"docker\" (got %q)", c.Runner))
	}
	switch c.Network {
	case "", AgentNetworkPerTier, AgentNetworkKernelOnlyAll:
	default:
		errs = append(errs, fmt.Sprintf("execution.agents.sandbox.network must be %q or %q (got %q)",
			AgentNetworkPerTier, AgentNetworkKernelOnlyAll, c.Network))
	}
	if c.Enabled && c.Runner == "docker" && strings.TrimSpace(c.DockerImage) == "" {
		errs = append(errs, "execution.agents.sandbox.docker_image is required when runner is \"docker\"")
	}
	for _, p := range c.AllowlistCap {
		if !filepath.IsAbs(p) && !strings.HasPrefix(p, "~/") && !strings.HasPrefix(p, "/") {
			errs = append(errs, fmt.Sprintf("execution.agents.sandbox.allowlist_cap entries must be absolute paths or start with ~/ (got %q)", p))
		}
	}
	for _, p := range c.HidePaths {
		if strings.TrimSpace(p) == "" {
			errs = append(errs, "execution.agents.sandbox.hide_paths must not contain an empty path")
		}
	}
	return errs
}
