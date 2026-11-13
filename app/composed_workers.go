package app

import (
	"log/slog"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/metabolism/compose"
	"github.com/cambrian-sh/core/internal/metabolism/dispatch"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
)

// composedRuntimeID is the id the ADR-0145 D12 composed runtime is registered
// under, or "" when execution.workers.composed is off (then it is not registered,
// and the planner's agent list is byte-identical to the legacy one).
func composedRuntimeID(c config.WorkersConfig) string {
	if !c.Composed {
		return ""
	}
	return c.RuntimeID()
}

// composerAgents is what the composer reads about registered agents.
type composerAgents interface {
	compose.AgentLookup
	compose.ManifestReader
}

// wireComposedWorkers threads execution.workers.composed to its consumers in one
// place (ADR-0145 D12; T-20261108-adr-0145-d12-kernel-composes-plan-step-workers):
//
//   - the Dispatcher gets a Composer: a plan step whose selected executor is a
//     profile agent runs on the composed runtime with a composition bound to its
//     lease (code and service agents are dispatched as before);
//   - the ToolExecutor's per-lease ceiling becomes the D12 subagent toolsets AND
//     the compositions (a composed lease may call only its composition's tools,
//     also under tools_unrestricted and over a loaded skill's grants);
//   - the Server serves each composition as the lease's menu and skill index
//     and forgets it when the lease completes; the runtime agent gets nothing
//     without a composed lease.
//
// Called after the run ledger (wireEvidencePipeline) and
// the skill access service are wired. Off ⇒ nothing is touched.
func wireComposedWorkers(cfg config.ExecutionConfig, server *subnetwork.Server, d *dispatch.Dispatcher, agents composerAgents) {
	// ADR-0145 D14 worker delegation (any worker, composed or not): the server
	// marks a step lease whose worker waits on delegate_to_planner and the
	// dispatcher neither times that step out nor re-dispatches it
	// (T-20261109-a-plan-step-is-re-dispatched-to-a-legacy-agent).
	if server != nil && d != nil && server.WorkerDelegations == nil {
		server.WorkerDelegations = domain.NewActiveDelegations()
		d.Delegations = server.WorkerDelegations
	}
	w := cfg.Workers
	if !w.Composed {
		return
	}
	if server == nil || server.ToolExecutor == nil || d == nil || agents == nil {
		slog.Error("ADR-0145 D12: execution.workers.composed is set but the server, tool executor or dispatcher is missing; composed workers are OFF")
		return
	}
	te := server.ToolExecutor
	leases := domain.NewComposedLeases()
	te.LeaseCeiling = domain.ChainLeaseCeilings(te.LeaseCeiling, leases)
	if te.Overlay == nil {
		te.Overlay = domain.NewRunGrantOverlay()
	}
	server.Compositions = leases
	server.ComposedRuntimeID = w.RuntimeID()

	c := &compose.Composer{
		RuntimeID: w.RuntimeID(),
		Agents:    agents,
		Manifests: agents,
		Tools:     te,
		Scope:     te.ScopeAllows,
		MaxSkills: w.SkillCap(),
		Leases:    leases,
		Overlay:   te.Overlay,
	}
	if server.SkillRegistry != nil {
		c.Skills = server.SkillRegistry
	}
	if server.SkillAccess != nil {
		c.Grants = server.SkillAccess
	}
	if server.Ledger != nil && server.Ledger.Writer != nil {
		c.Ledger = server.Ledger.Writer
	} else {
		slog.Error("ADR-0145 D12: composed workers are on but the run ledger is not wired; compositions are NOT recorded")
	}
	// find_tools may report tools outside a step's composition, never load them
	// (T-20261112-yield-sub-goal-on-a-composed-lease-runs-under): the bound is every
	// registered profession's allowlist, so the registry must list agents.
	if fleet, ok := agents.(compose.AgentLister); ok {
		c.Fleet = fleet
		server.ToolsOutsideStep = c.OutsideStep
	} else {
		slog.Warn("ADR-0145 D12: the agent registry cannot list agents; find_tools reports no tools outside a step")
	}
	d.Composer = c
	// ADR-0150 D1: the single-agent lane composes a whole request on the same
	// runtime when execution.lane.single_agent.agent_id is empty.
	server.RequestComposer = c
	slog.Info("ADR-0145 D12: composed workers ON", "runtime", w.RuntimeID(), "max_skills", w.SkillCap(),
		"ledger", c.Ledger != nil)
}
