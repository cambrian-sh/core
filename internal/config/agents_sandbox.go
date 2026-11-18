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
// same rule as the agent card): a composition-root system agent is system; a
// local agent that declares no tier is first-party; a profile's or a local
// agent manifest's declared trust_tier is kept (a manifest may lower privilege,
// never raise it: storage.manifestTrustTier, ENG-2085).
//
// enabled: true (the default since ADR-0157 S7, owner decision 2026-11-17,
// after the live run on the production backend, ENG-2010) — ADR-0157 S1: every
// agent of every tier launches through
// internal/sandbox (sandbox.StartCmd) with D1 confinement: a private ephemeral
// scratch directory (its TEMP/TMP/TMPDIR/HOME, and on Windows its
// USERPROFILE/APPDATA/LOCALAPPDATA), its socket in a per-instance
// directory under work_root, and the kernel's own state (the kernel directory,
// configs/ with config.db and secret.key, the data directory, backups, the MCP
// and agent work roots, plus hide_paths) unreadable where the runner can do it.
// A control the runner cannot enforce is MISSING and the tier's on_unavailable
// decides; the boot log names enforced and missing controls per agent.
//
// enabled: false — the launch is unchanged from before ADR-0157: the env
// allowlist (procenv), the SEC-01 memory cap, a lifetime-bound process, the
// socket under the OS temp directory; a third-party agent is refused unless
// third_party.on_unavailable is degrade (ENG-716 stage 1).
//
// The S7 flip needs no schema migration: no key moved or was removed, and the
// owner's decision is that installs which never set the key get the sandbox.
// An operator who set enabled: false keeps it — the key carries no omitempty,
// so an explicit false survives a marshal of the effective config (the
// defaults layer, the ADR-0101 store), exactly like execution.erasure.enabled.
type AgentSandboxConfig struct {
	// Enabled launches agents through the sandbox (ADR-0157 S1). Default true
	// (S7).
	Enabled bool `json:"enabled"`
	// Runner: "native" (default; Linux bubblewrap, Windows Job Object + an
	// AppContainer token — low integrity when a runtime path cannot be granted —,
	// macOS a Seatbelt profile; ADR-0157 S5) or "docker" (Linux hosts: one
	// container per agent instance).
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
	// (no network namespace / --network none / no AppContainer network
	// capability / a Seatbelt network deny; they reach the kernel through a unix
	// socket in their socket directory), first-party and system agents keep
	// the normal network (on Windows without loopback: the AppContainer reaches
	// the kernel through the socket too); "kernel_only_all" — every agent is kernel-only, for
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

	// Resource limits (ENG-2112, owner decision 2026-11-18): first-party and
	// system agents are capped too — their chat and daemon workers are
	// long-lived. Exceeding a limit kills the instance into the restart /
	// quarantine policy with the reason logged. 0 ⇒ the default. Defaults are
	// sized from the shipped agents' measured peaks on Windows (ENG-2112 log):
	// the reranker (torch, bge-reranker-v2-m3) 5.8 GB committed, kg_extractor
	// (spaCy) 0.8 GB, docling 0.8 GB, each ONE process.
	//
	// MemoryMB caps the agent's process tree (Windows: job committed memory;
	// Linux: cgroup memory.max, else a per-process data limit plus the tree's
	// resident total watched; docker --memory). A smaller manifest
	// memory_limit_mb (or agent_memory_limit_mb) still applies; a larger one
	// does not lift it. 0 ⇒ 8192.
	MemoryMB int `json:"memory_mb,omitempty"`
	// MaxProcesses is the tree's process count (Windows: the job's active
	// process limit; Linux: the limit watch). 0 ⇒ 32.
	MaxProcesses int `json:"max_processes,omitempty"`
	// MaxTasks is the Linux hard ceiling on processes AND threads (cgroup
	// pids.max, ulimit -u in bubblewrap's user namespace, docker
	// --pids-limit): torch runs dozens of threads in one process, so it is set
	// apart from MaxProcesses. Unused on Windows and macOS. 0 ⇒ 1024.
	MaxTasks int `json:"max_tasks,omitempty"`
	// ScratchMB is the instance's scratch directory quota (size accounting,
	// every OS). Model weights live in execution.agents.model_cache_dir, not in
	// the scratch. 0 ⇒ 4096.
	ScratchMB int `json:"scratch_mb,omitempty"`
}

// Refuses reports whether a missing control refuses a first-party launch.
func (t AgentFirstPartySandbox) Refuses() bool { return t.OnUnavailable == "refuse" }

// Memory is MemoryMB with its default applied.
func (t AgentFirstPartySandbox) Memory() int { return orDefault(t.MemoryMB, 8192) }

// Processes is MaxProcesses with its default applied.
func (t AgentFirstPartySandbox) Processes() int { return orDefault(t.MaxProcesses, 32) }

// Tasks is MaxTasks with its default applied.
func (t AgentFirstPartySandbox) Tasks() int { return orDefault(t.MaxTasks, 1024) }

// Scratch is ScratchMB with its default applied.
func (t AgentFirstPartySandbox) Scratch() int { return orDefault(t.ScratchMB, 4096) }

// AgentTierSandbox is one tier's agent containment.
type AgentTierSandbox struct {
	// OnUnavailable: "refuse" or "degrade" ("" ⇒ refuse: fail closed).
	OnUnavailable string `json:"on_unavailable,omitempty"`
	// MemoryMB is the tier's memory cap; a smaller per-agent manifest
	// memory_limit_mb still applies, a larger one does not lift it. 0 ⇒ 2048.
	MemoryMB int `json:"memory_mb,omitempty"`
	// MaxProcesses, MaxTasks and ScratchMB mean what they mean for the first
	// party (AgentFirstPartySandbox), with tighter defaults: 16 processes,
	// 512 tasks, a 1024 MiB scratch (ENG-2112).
	MaxProcesses int `json:"max_processes,omitempty"`
	MaxTasks     int `json:"max_tasks,omitempty"`
	ScratchMB    int `json:"scratch_mb,omitempty"`
}

// Refuses reports whether a missing control refuses the launch.
func (t AgentTierSandbox) Refuses() bool { return t.OnUnavailable != "degrade" }

// Memory is MemoryMB with its default applied.
func (t AgentTierSandbox) Memory() int { return orDefault(t.MemoryMB, 2048) }

// Processes is MaxProcesses with its default applied.
func (t AgentTierSandbox) Processes() int { return orDefault(t.MaxProcesses, 16) }

// Tasks is MaxTasks with its default applied.
func (t AgentTierSandbox) Tasks() int { return orDefault(t.MaxTasks, 512) }

// Scratch is ScratchMB with its default applied.
func (t AgentTierSandbox) Scratch() int { return orDefault(t.ScratchMB, 1024) }

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
	for _, l := range []struct {
		key string
		v   int
	}{
		{"third_party.memory_mb", c.ThirdParty.MemoryMB},
		{"third_party.max_processes", c.ThirdParty.MaxProcesses},
		{"third_party.max_tasks", c.ThirdParty.MaxTasks},
		{"third_party.scratch_mb", c.ThirdParty.ScratchMB},
		{"first_party.memory_mb", c.FirstParty.MemoryMB},
		{"first_party.max_processes", c.FirstParty.MaxProcesses},
		{"first_party.max_tasks", c.FirstParty.MaxTasks},
		{"first_party.scratch_mb", c.FirstParty.ScratchMB},
	} {
		if l.v < 0 {
			errs = append(errs, fmt.Sprintf("execution.agents.sandbox.%s must be >= 0 (0 = the default)", l.key))
		}
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
