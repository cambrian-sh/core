package app

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/kernel"
)

// registerAgentSources discovers and upserts agent definitions from plugin AgentSources
// (ADR-0075). Parallel to registerModelAgents: each definition goes through the same
// reg.SetAgent path as any other agent, so it participates in the scope/merit/dispatch
// machinery normally. A System=true definition (from Registry.AddSystemAgent) is logged
// as an explicit privilege grant — the grant is visible, not inferred.
//
// It returns every agent it registered, so the composition root can bind what a
// declarative profile declares (tool grants and ceiling, model preference) and
// prune profile agents that were not discovered this boot.
//
// A source that refused agents at discovery (refusingAgentSource: a manifest
// trust_tier the kernel will not apply as written, ENG-2085) has their earlier
// registrations deleted once every source has registered — unless another
// source registered the same id this boot — so a refused agent cannot stay
// launchable at the tier it had before.
func registerAgentSources(ctx context.Context, reg *kernel.AgentRepoDecorator, sources []AgentSource) []DiscoveredAgent {
	var registered []DiscoveredAgent
	var refused []string
	for _, src := range sources {
		discovered, err := src.DiscoverAgents(ctx)
		if rs, ok := src.(refusingAgentSource); ok {
			refused = append(refused, rs.RefusedAgentIDs()...)
		}
		if err != nil {
			slog.Warn("registerAgentSources: source discovery failed", "source", src.Name(), "err", err)
			continue
		}
		for _, da := range discovered {
			def := da.Definition
			if def.System {
				slog.Warn("ADR-0075: registering PRIVILEGED system agent (explicit grant)",
					"source", src.Name(), "id", def.ID)
			}
			// SetAgentWithManifest carries the manifest EXTRAS (PythonDeps/MemoryLimitMB/
			// schemas); a nil manifest degrades to a record-only write.
			if err := reg.SetAgentWithManifest(def, da.Manifest); err != nil {
				slog.Warn("registerAgentSources: failed to register agent", "source", src.Name(), "id", def.ID, "err", err)
				continue
			}
			slog.Info("ADR-0075: registered agent from source", "source", src.Name(), "id", def.ID, "system", def.System, "manifest", da.Manifest != nil)
			registered = append(registered, da)
		}
	}
	pruneRefusedAgents(ctx, reg, refused, registered)
	return registered
}

// refusingAgentSource is an AgentSource that reports the agents it found but
// refused to register (FilesystemAgentSource).
type refusingAgentSource interface {
	RefusedAgentIDs() []string
}

// pruneRefusedAgents deletes the registry records of agents discovery refused
// (ENG-2085), sparing an id another source registered this boot.
func pruneRefusedAgents(ctx context.Context, reg domain.AgentPruner, refused []string, registered []DiscoveredAgent) {
	if len(refused) == 0 {
		return
	}
	kept := make(map[string]bool, len(registered))
	for _, da := range registered {
		kept[da.Definition.ID] = true
	}
	for _, id := range refused {
		if kept[id] {
			continue
		}
		if err := reg.DeleteAgent(ctx, id); err != nil {
			slog.Warn("registerAgentSources: dropping a refused agent's registration failed", "id", id, "err", err)
			continue
		}
		slog.Warn("registerAgentSources: refused agent is not registered; any earlier registration was dropped (ENG-2085)", "id", id)
	}
}

// registerModelAgents persists each generator as a TraitModel AgentDefinition so it is
// an addressable agent in the registry (dispatch, scope, merit). ADR-0042: sourced from
// llm_provider.generators, agent ID = "llm:<id>" (id-keyed end to end; the server strips
// "llm:" to the generator id when Acquiring an agent-step model).
func registerModelAgents(reg *kernel.AgentRepoDecorator, generators []config.GeneratorConfig) {
	for _, g := range generators {
		agentID := "llm:" + g.ID
		def := domain.AgentDefinition{
			ID:              agentID,
			Name:            g.Model,
			Description:     fmt.Sprintf("LLM inference provider: %s / %s. Capabilities: %v", g.Provider, g.Model, g.Capabilities),
			Runtime:         domain.RuntimeBinary,
			Trait:           domain.TraitModel,
			Provisional:     false,
			ManifestVersion: "1.0.0",
		}
		if err := reg.SetAgent(def); err != nil {
			slog.Warn("registerModelAgents: failed to register model", "id", agentID, "err", err)
		}
		slog.Info("Registered model agent", "id", agentID, "trait", domain.TraitModel)
	}
}

// registryReconciler is the minimal registry surface the startup reconcilers
// need: list the catalogue and evict an orphan. *kernel.AgentRepoDecorator
// satisfies it; a fake satisfies it in tests.
type registryReconciler interface {
	GetAllAgents(ctx context.Context) ([]domain.AgentDefinition, error)
	domain.AgentPruner
}

// reconcileModelAgents evicts TraitModel agents no longer declared in
// config.Generators. registerModelAgents only UPSERTS the current generators, so
// a model removed from config would otherwise linger in the registry (bbolt) and
// keep winning the auction after a restart — its persisted merit beats a
// cold-start replacement, and it routes to a backend that may be gone (the
// "model_unavailable: all candidates degraded" failure mode). Config is the
// declarative source of truth for the model population, so any Trait==model agent
// whose id is not "llm:<generator-id>" is an orphan and is pruned. Non-model
// agents are untouched — they reconcile against their own sources (filesystem /
// A2A) and must never be pruned here.
func reconcileModelAgents(ctx context.Context, reg registryReconciler, generators []config.GeneratorConfig) {
	declared := make(map[string]bool, len(generators))
	for _, g := range generators {
		declared["llm:"+g.ID] = true
	}
	agents, err := reg.GetAllAgents(ctx)
	if err != nil {
		slog.Warn("reconcileModelAgents: list agents failed; skipping prune", "err", err)
		return
	}
	for _, a := range agents {
		if a.Trait != domain.TraitModel || declared[a.ID] {
			continue
		}
		if err := reg.DeleteAgent(ctx, a.ID); err != nil {
			slog.Warn("reconcileModelAgents: prune failed", "id", a.ID, "err", err)
			continue
		}
		slog.Info("reconcileModelAgents: pruned orphaned model no longer in config", "id", a.ID)
	}
}

// reconcileFilesystemAgents evicts agents whose local source file no longer
// exists. The bbolt seeder (storage.Seed) is upsert-only — it seeds new agent
// files and updates changed ones but never removes an agent whose *.py / sidecar
// manifest was deleted, so the stale record keeps competing in the auction (the
// same orphan class as models, with the filesystem as the declarative source).
//
// Provenance scoping is critical: this prunes ONLY filesystem-sourced agents
// (those carrying a local ExecPath). It deliberately spares
//   - TraitModel agents      — reconciled against config by reconcileModelAgents;
//   - RuntimeA2A agents       — registered dynamically at runtime with no local
//     source file, so absence-on-disk says nothing about their liveness;
//   - any record without an ExecPath — nothing to check against.
//
// exists reports whether an agent's source path is still present (injected for
// testability; os.Stat at the call site).
func reconcileFilesystemAgents(ctx context.Context, reg registryReconciler, exists func(path string) bool) {
	agents, err := reg.GetAllAgents(ctx)
	if err != nil {
		slog.Warn("reconcileFilesystemAgents: list agents failed; skipping prune", "err", err)
		return
	}
	for _, a := range agents {
		if a.Trait == domain.TraitModel || a.Runtime == domain.RuntimeA2A || a.ExecPath == "" {
			continue
		}
		// ExecPath is stored BOTH ways: the filesystem scanner records a path
		// relative to Dir, while a plugin-contributed definition carries an
		// absolute one — it has to, because the spawner runs it with
		// cmd.Dir = Dir and no other anchor.
		//
		// Joining an already-absolute path onto Dir produced a doubled path that
		// can never exist, so every plugin-contributed agent was deleted at the
		// same boot that registered it, and every later spawn failed with
		// "agent not found in database". That is what kept a second Telegram bot
		// from ever starting.
		// An empty Dir is skipped for the same reason: there is nothing to
		// resolve against, so joining can only rewrite separators — enough, on
		// Windows, to turn a path that exists into one that does not.
		fullPath := a.ExecPath
		if a.Dir != "" && !filepath.IsAbs(fullPath) {
			fullPath = filepath.Join(a.Dir, a.ExecPath)
		}
		if exists(fullPath) {
			continue
		}
		if err := reg.DeleteAgent(ctx, a.ID); err != nil {
			slog.Warn("reconcileFilesystemAgents: prune failed", "id", a.ID, "err", err)
			continue
		}
		slog.Info("reconcileFilesystemAgents: pruned agent whose source file is gone",
			"id", a.ID, "exec_path", a.ExecPath)
	}
}
