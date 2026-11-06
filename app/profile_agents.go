package app

import (
	"context"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/cambrian-sh/core/domain"
	skilldiscovery "github.com/cambrian-sh/core/internal/skill/discovery"
	"github.com/cambrian-sh/core/internal/storage"
)

// Agent stack v2, part B (T-20261105-agent-stack-v2-worker-agent-runtime-plus;
// owner directive decisions/agent-stack-v2-mcp-tools-and-profiles.md D3; the
// contract is agents/profiles/README.md "Part B").
//
// Discovery registers one agent per declarative profile (storage/
// profile_discovery.go). This file binds what a profile declares to the kernel:
//
//   - tools: the allowlist is EXPANDED against the tool registry into the agent's
//     ToolGrants, and re-expanded whenever an MCP server's tools change (the MCP
//     tool sink calls profileGrantSync.Sync), because MCP tools register after
//     boot. The same globs are the agent's hard tool CEILING
//     (domain.ProfileToolCeiling), which holds under tools_unrestricted too;
//   - model: the LLM gateway puts the preferred generator first on the agent's
//     lease chain (profileModelPreference);
//   - trust_tier: stored on the definition; the ADR-0139 D8 containment mapping
//     is not built, which boot logs say.

// profileAgent is one registered profile agent, as the composition root binds it.
type profileAgent struct {
	ID        string
	Allow     domain.ToolAllowlist
	Model     string
	TrustTier string
	// Skills is the profile's `skills` list: with the agent's grants, its
	// effective skill set (decisions/skill-access-grants.md, app/skill_access_wiring.go).
	Skills []string
}

// profileSkillIndex is the skill index profiles are validated against: the
// skills this kernel serves (name → the tools each grants). A missing or
// unreadable directory yields an empty index, which makes every profile that
// lists a skill invalid — loudly, at boot — rather than registering agents whose
// skills the kernel cannot serve.
func profileSkillIndex(dir string) map[string][]string {
	skills, err := skilldiscovery.Discover(dir)
	if err != nil {
		slog.Error("agent profiles: skills directory unreadable; profiles that list skills will be refused",
			"dir", dir, "err", err)
	}
	out := make(map[string][]string, len(skills))
	for _, s := range skills {
		out[s.Name] = append([]string{}, s.ToolGrants...)
	}
	return out
}

// collectProfileAgents picks the profile agents out of what registration
// registered, sorted by id.
func collectProfileAgents(registered []DiscoveredAgent) []profileAgent {
	var out []profileAgent
	for _, da := range registered {
		def := da.Definition
		if def.Profile == "" {
			continue
		}
		pa := profileAgent{ID: def.ID, Model: def.ModelPreference, TrustTier: def.TrustTier}
		if da.Manifest != nil {
			pa.Allow = append(domain.ToolAllowlist(nil), da.Manifest.ToolAllowlist...)
			pa.Skills = append([]string{}, da.Manifest.Skills...)
		}
		out = append(out, pa)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > 0 {
		tiers := make([]string, 0, len(out))
		for _, p := range out {
			tiers = append(tiers, p.ID+"="+p.TrustTier)
		}
		// Say it at boot rather than let a declared tier look enforced.
		slog.Warn("agent profiles: trust_tier is recorded but NOT yet enforced as process containment "+
			"(ADR-0139 D8 containment tiers are not built); every profile agent runs in the standard SEC-01 sandbox",
			"profiles", strings.Join(tiers, ","))
	}
	return out
}

// reconcileProfileAgents prunes records that run the profile runtime but were not
// registered this boot: a profile deleted, renamed or now invalid, and the
// pre-part-B registration of worker_agent.py itself. The exec-path reconcile
// cannot catch them — their exec path, the runtime, still exists — so without
// this an invalid profile's last good registration would keep being routed to,
// and a runtime refusing it at boot would never answer.
func reconcileProfileAgents(ctx context.Context, reg registryReconciler, registered []DiscoveredAgent) {
	keep := make(map[string]bool, len(registered))
	for _, da := range registered {
		keep[da.Definition.ID] = true
	}
	agents, err := reg.GetAllAgents(ctx)
	if err != nil {
		slog.Warn("reconcileProfileAgents: list agents failed; skipping prune", "err", err)
		return
	}
	for _, a := range agents {
		if keep[a.ID] || !isProfileRuntime(a) {
			continue
		}
		if err := reg.DeleteAgent(a.ID); err != nil {
			slog.Warn("reconcileProfileAgents: prune failed", "id", a.ID, "err", err)
			continue
		}
		slog.Info("reconcileProfileAgents: pruned a profile-runtime agent no longer discovered "+
			"(profile removed or invalid, or the bare worker_agent.py registration)", "id", a.ID)
	}
}

func isProfileRuntime(a domain.AgentDefinition) bool {
	return a.Profile != "" || filepath.Base(filepath.FromSlash(a.ExecPath)) == storage.ProfileRuntimeFile
}

// profileGrantSync keeps profile agents' ToolGrants equal to their allowlists'
// expansion over the live tool registry.
//
// An operator's own grant (SetToolGrant) on a profile agent is preserved across a
// re-sync, but the profile ceiling still bounds it: a profile is widened by
// editing the profile, not by a grant.
type profileGrantSync struct {
	grants *domain.InMemoryGrantsStore
	tools  func() []domain.SystemTool

	mu     sync.Mutex
	agents []profileAgent
	// derived is, per agent, the tools the last sync granted from the allowlist —
	// so a re-sync can tell its own grants from an operator's.
	derived map[string]map[string]bool
}

func newProfileGrantSync(grants *domain.InMemoryGrantsStore, tools func() []domain.SystemTool, agents []profileAgent) *profileGrantSync {
	return &profileGrantSync{grants: grants, tools: tools, agents: agents, derived: map[string]map[string]bool{}}
}

// Sync re-expands every profile agent's allowlist. boot=true also warns about each
// glob that matches no registered tool (the profile names a tool no connected
// server lists — a wrong server id, or a server that is down).
func (s *profileGrantSync) Sync(ctx context.Context, boot bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tools := s.tools()
	for _, pa := range s.agents {
		names := pa.Allow.Expand(tools)
		want := make(map[string]bool, len(names))
		for _, n := range names {
			want[n] = true
		}
		current, _ := s.grants.GrantsFor(ctx, pa.ID)
		prev := s.derived[pa.ID]
		next := make([]domain.ToolGrant, 0, len(names)+len(current))
		have := map[string]bool{}
		for _, g := range current {
			switch {
			case want[g.Tool]:
				next = append(next, g) // keep its policy
				have[g.Tool] = true
			case prev[g.Tool]:
				// derived last time, no longer matched: drop
			default:
				next = append(next, g) // an operator's grant
			}
		}
		for _, n := range names {
			if !have[n] {
				next = append(next, domain.ToolGrant{Tool: n})
			}
		}
		s.grants.Set(pa.ID, next)
		s.derived[pa.ID] = want
		if boot {
			if unmatched := pa.Allow.Unmatched(tools); len(unmatched) > 0 {
				slog.Warn("agent profile: tool allowlist entries match no registered tool "+
					"(wrong server id, or the server is not connected); they grant nothing until a tool matches",
					"profile", pa.ID, "unmatched", unmatched)
			}
		}
		slog.Info("agent profile: tool grants = allowlist expansion", "profile", pa.ID, "granted", len(names), "boot", boot)
	}
}

// profileToolCeiling builds the hard ceiling for every profile agent.
func profileToolCeiling(agents []profileAgent) *domain.ProfileToolCeiling {
	c := domain.NewProfileToolCeiling()
	for _, pa := range agents {
		c.Set(pa.ID, pa.Allow)
	}
	return c
}

// profileModelPreference resolves a profile agent's `model:` per call: a
// generator id as is, `role:<role>` through the live role table. "" means no
// preference (the lease chain is unchanged).
func profileModelPreference(agents []profileAgent, roles func() map[string]string) func(agentID string) string {
	pref := make(map[string]string, len(agents))
	for _, pa := range agents {
		if pa.Model != "" {
			pref[pa.ID] = pa.Model
		}
	}
	if len(pref) == 0 {
		return nil
	}
	return func(agentID string) string {
		m := pref[agentID]
		if role, ok := strings.CutPrefix(m, "role:"); ok {
			if roles == nil {
				return ""
			}
			return roles()[role]
		}
		return m
	}
}

// checkProfileModels warns at boot about a preference that resolves to nothing:
// a generator id that is not configured, or a role with no assignment. The agent
// still runs — on its step allocation and the default — exactly as a dangling
// role assignment falls back; the warning is so the fallback is not silent.
func checkProfileModels(agents []profileAgent, known func(generatorID string) bool, roles map[string]string) {
	for _, pa := range agents {
		if pa.Model == "" {
			continue
		}
		id := pa.Model
		if role, ok := strings.CutPrefix(pa.Model, "role:"); ok {
			id = roles[role]
			if id == "" {
				slog.Warn("agent profile: model preference names a role with no generator assigned; "+
					"the agent uses the default model", "profile", pa.ID, "model", pa.Model, "resolved", false)
				continue
			}
		}
		if known == nil || !known(id) {
			slog.Warn("agent profile: model preference is not a configured generator; "+
				"the agent uses the default model", "profile", pa.ID, "model", pa.Model, "generator", id, "resolved", false)
			continue
		}
		slog.Info("agent profile: model preference bound", "profile", pa.ID, "model", pa.Model, "generator", id, "resolved", true)
	}
}
