package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/skill/lifecycle"
)

// lifecycleConfig maps execution.skills onto the lifecycle's tuning.
func lifecycleConfig(c config.SkillsConfig) lifecycle.Config {
	return lifecycle.Config{
		ProposalsEnabled:   c.ProposalsEnabled,
		MaxPendingPerAgent: c.MaxPendingPerAgent,
		MaxSkillMDChars:    c.MaxSkillMDChars,
		Curator: lifecycle.CuratorConfig{
			Enabled:      c.Curator.Enabled,
			StaleAfter:   time.Duration(c.Curator.StaleAfterDays) * 24 * time.Hour,
			ArchiveAfter: time.Duration(c.Curator.ArchiveAfterDays) * 24 * time.Hour,
			Interval:     time.Duration(c.Curator.IntervalMinutes) * time.Minute,
			Sources:      c.Curator.Sources,
			Pinned:       c.Curator.Pinned,
		},
	}
}

// wireSkillLifecycle builds the ADR-0139 D13 skill lifecycle over the Postgres
// store and overlays it on the file-discovered catalog: shipped skills get
// lifecycle records (SyncBuiltins), approved agent skills are materialised into
// reg and the index, archived skills are withdrawn. Returns (nil, nil) — and the
// "skills-lifecycle" capability stays unadvertised — when there is no pool or
// the 0029 tables are missing (an un-migrated database), so a console never
// renders a queue whose RPCs answer Unimplemented.
//
// No pipeline or schedule in OSS references a skill by name today, so the
// curator's reference exemption has no source (refs = nil); the "pinned" list in
// execution.skills.curator is the exemption an operator controls.
//
// grants receives each approved proposal's proposer grant
// (decisions/skill-access-grants.md); nil when no grant store is wired, which is
// logged, because an approved skill then reaches no profile agent.
func wireSkillLifecycle(ctx context.Context, cfg config.SkillsConfig, pool *pgxpool.Pool,
	reg *domain.InMemorySkillRegistry, idx lifecycle.Indexer, discovered []domain.Skill,
	grants lifecycle.ProposalGranter) (*lifecycle.Service, *lifecycle.Curator) {
	if pool == nil {
		return nil, nil
	}
	store := postgres.NewPgSkillLifecycleStore(pool)
	if !store.Ready(ctx) {
		slog.Warn("ADR-0139 D13: skill lifecycle tables missing (run `orchestrator migrate up`); skill proposals disabled")
		return nil, nil
	}
	if grants == nil {
		slog.Warn("skill access: no skill grant store (migration 0031); an approved skill proposal is not granted to its proposer")
	}
	svc, err := lifecycle.New(store, reg, idx, lifecycleConfig(cfg), nil, nil, grants)
	if err != nil {
		slog.Error("ADR-0139 D13: skill lifecycle not built", "err", err)
		return nil, nil
	}
	if err := svc.SyncBuiltins(ctx, discovered); err != nil {
		slog.Warn("ADR-0139 D13: shipped skills not fully recorded in the lifecycle", "err", err)
	}
	if err := svc.Materialize(ctx); err != nil {
		slog.Warn("ADR-0139 D13: some approved skills could not be served", "err", err)
	}
	var curator *lifecycle.Curator
	if cfg.Curator.Enabled {
		curator = lifecycle.NewCurator(svc, 0, nil)
	}
	slog.Info("ADR-0139 D13: skill lifecycle wired",
		"proposals_enabled", cfg.ProposalsEnabled, "curator", cfg.Curator.Enabled)
	return svc, curator
}
