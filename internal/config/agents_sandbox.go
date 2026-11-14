package config

import "fmt"

// AgentSandboxConfig is execution.agents.sandbox: how a kernel-spawned agent
// process is contained by its ADR-0139 D8 trust tier (owner decision 2026-11-14,
// T-20261106-kernel-spawned-agents-are-not-contained-by: a new block, per-tier,
// first-party unchanged, third-party contained). It is the agents' counterpart of
// mcp.sandbox (MCP stdio servers): agents are not MCP servers, so they get their
// own block (AGENTS.md §3.4), with the same tier and on_unavailable vocabulary.
//
// The tier an agent runs at is the kernel's view (agentmgr.agentTrustTier, the
// same rule as the agent card): a composition-root system agent and a local agent
// that declares no tier are first-party; a profile's declared trust_tier is kept.
//
// first-party / system: unchanged — the env allowlist (procenv), the SEC-01
// memory cap (agent_memory_limit_mb or the manifest's), a lifetime-bound process.
//
// third-party: contained. The agent launch path today delivers the env allowlist
// and a memory cap, NOT write confinement, network denial or a low-privilege
// identity (stages S2/S3 of the ticket). Those controls are therefore MISSING for
// a third-party agent, and on_unavailable decides, as for an MCP server:
// "refuse" (the default; the agent does not start) or "degrade" (it starts with
// the env allowlist and the tier memory cap, and the boot logs what is missing).
type AgentSandboxConfig struct {
	ThirdParty AgentTierSandbox `json:"third_party"`
}

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

// validate checks execution.agents.sandbox.
func (c AgentSandboxConfig) validate() []string {
	var errs []string
	switch c.ThirdParty.OnUnavailable {
	case "", "refuse", "degrade":
	default:
		errs = append(errs, fmt.Sprintf("execution.agents.sandbox.third_party.on_unavailable must be \"refuse\" or \"degrade\" (got %q)", c.ThirdParty.OnUnavailable))
	}
	if c.ThirdParty.MemoryMB < 0 {
		errs = append(errs, "execution.agents.sandbox.third_party.memory_mb must be >= 0 (0 = 2048)")
	}
	return errs
}
