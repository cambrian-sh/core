package agentmgr

import (
	"context"
	"fmt"
	"log/slog"
	goruntime "runtime"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// The ADR-0139 D8 tiers an agent process can run at (agentprofile.TrustTiers).
const (
	agentTierSystem     = "system"
	agentTierFirstParty = "first-party"
	agentTierThirdParty = "third-party"
)

// AgentContainment is execution.agents.sandbox as the instance manager applies
// it (owner decision 2026-11-14, T-20261106-kernel-spawned-agents-are-not-
// contained-by). Its ZERO VALUE is the safe one: a third-party agent is refused
// (fail closed) with the default 2048 MB cap, so a composition root that never
// calls SetAgentContainment still contains third-party agents.
type AgentContainment struct {
	// ThirdPartyDegrade starts a third-party agent although controls are
	// missing (on_unavailable: degrade); false refuses it (the default).
	ThirdPartyDegrade bool
	// ThirdPartyMemoryMB is the third-party memory cap; 0 ⇒ 2048.
	ThirdPartyMemoryMB int
	// Sandbox, when non-nil (execution.agents.sandbox.enabled), launches every
	// agent of every tier through internal/sandbox with ADR-0157 D1
	// confinement (agent_sandbox.go). nil keeps the launch exactly as before.
	Sandbox *AgentSandbox
}

func (c AgentContainment) thirdPartyMemoryMB() int {
	if c.ThirdPartyMemoryMB > 0 {
		return c.ThirdPartyMemoryMB
	}
	return 2048
}

// SetAgentContainment configures the per-tier agent containment.
func (im *InstanceManager) SetAgentContainment(c AgentContainment) { im.containment = c }

// agentTrustTier is the tier the kernel runs an agent at — the kernel's view,
// never the agent's claim, by the same rule as the agent card
// (agentcard.TrustTier): a composition-root system agent is system; a profile
// claiming "system" is first-party; a declared tier is kept; a local agent with
// no declared tier is first-party (it runs from the deployment's own agents
// directory).
func agentTrustTier(def *domain.AgentDefinition) string {
	switch {
	case domain.IsSystemDefinition(*def):
		return agentTierSystem
	case def.TrustTier == agentTierSystem, def.TrustTier == "":
		return agentTierFirstParty
	}
	return def.TrustTier
}

// thirdPartyMissing lists the third-party controls the UNSANDBOXED agent launch
// path cannot deliver: write confinement, network denial, a low-privilege
// identity — and the memory cap where this OS has no resource caps
// (process_caps_other.go). With the agent sandbox on, every one of them is the
// sandbox's to enforce or report (admitLaunch).
func (im *InstanceManager) thirdPartyMissing() []string {
	missing := []string{"fs_confine", "network", "low_privilege"}
	if goruntime.GOOS != "windows" && goruntime.GOOS != "linux" {
		missing = append(missing, "memory")
	}
	return missing
}

// admitLaunch decides whether an agent may start under its tier's containment
// and returns its memory cap. First-party and system agents are unchanged
// (the SEC-01 cap: manifest limit, else the global default). A third-party agent
// is refused while controls are missing, unless the operator chose degrade; then
// it runs with the env allowlist and the tier memory cap (a smaller manifest
// limit still applies; a larger one cannot lift the tier's).
//
// With the agent sandbox on (ADR-0157 S2) a third-party agent is ADMITTED here
// with the tier memory cap: D1 confinement, the low-privilege identity and the
// kernel-only network (D3) are the sandbox plan's to enforce, and the plan
// refuses — or, with degrade, starts and reports — over exactly the controls
// the runner lacks on this host (Windows: none in the AppContainer, ADR-0157
// S5; network and hidden reads when it falls back to low integrity).
func (im *InstanceManager) admitLaunch(ctx context.Context, def *domain.AgentDefinition) (memMB int, err error) {
	if agentTrustTier(def) != agentTierThirdParty {
		return im.spawnMemLimitMB(ctx, def), nil
	}
	if im.containment.Sandbox != nil {
		if goruntime.GOOS == "linux" && domain.IsJSRuntime(def.Runtime) {
			return 0, nil // RLIMIT_AS kills V8 at boot (spawnMemLimitMB)
		}
		memMB = im.containment.thirdPartyMemoryMB()
		if m := im.spawnMemLimitMB(ctx, def); m > 0 && m < memMB {
			memMB = m
		}
		return memMB, nil
	}
	missing := im.thirdPartyMissing()
	if !im.containment.ThirdPartyDegrade {
		return 0, fmt.Errorf("agent %s is third-party (ADR-0139 D8) and its containment is unavailable on the agent launch path (missing: %s); not started. "+
			"Set execution.agents.sandbox.third_party.on_unavailable to \"degrade\" to run it with the env allowlist and the tier memory cap only",
			def.ID, strings.Join(missing, ", "))
	}
	memMB = im.containment.thirdPartyMemoryMB()
	if goruntime.GOOS == "linux" && domain.IsJSRuntime(def.Runtime) {
		memMB = 0 // RLIMIT_AS kills V8 at boot (spawnMemLimitMB); reported below
		missing = append(missing, "memory")
	}
	if m := im.spawnMemLimitMB(ctx, def); m > 0 && m < memMB {
		memMB = m
	}
	slog.Warn("ADR-0139 D8: third-party agent started DEGRADED (execution.agents.sandbox.third_party.on_unavailable=degrade)",
		"agent_id", def.ID, "missing", missing, "memory_mb", memMB)
	return memMB, nil
}
