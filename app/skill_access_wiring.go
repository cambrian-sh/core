package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/skill/access"
)

// Per-agent skill access (owner decision 2026-11-05,
// cambrian-knowledge/decisions/skill-access-grants.md; amends ADR-0139 D13).
//
// A profile agent's effective skill set is its profile's `skills` list plus its
// durable grants (0031 tables). ListSkills (network.Server.SkillAccess) serves a
// body only inside the set; use_skill outside it asks the operator on the SAME
// approval hub dangerous tools use (toolApproval, not toolApprovalCtrl: the
// tools_auto_approve bypass is a decision about dangerous tools, so skill access
// has its own execution.skills.access_auto_approve).

// profileSkillSets resolves an agent id to its profile's skills. Only profile
// agents are bound (domain.ProfileToolCeiling's "bound" semantics).
func profileSkillSets(agents []profileAgent) access.ProfileSkills {
	sets := make(map[string][]string, len(agents))
	for _, pa := range agents {
		sets[pa.ID] = append([]string{}, pa.Skills...)
	}
	return func(agentID string) ([]string, bool) {
		s, ok := sets[agentID]
		return s, ok
	}
}

// wireSkillAccess builds the access service. It is always built, so profile
// agents are bounded even without Postgres; the grant store is attached only when
// the 0031 tables exist (else bound agents get exactly their profile skills and
// every request is denied with a reason — fail closed).
func wireSkillAccess(ctx context.Context, cfg config.SkillsConfig, pool *pgxpool.Pool, agents []profileAgent,
	reg domain.SkillRegistry, hub domain.ApprovalController) *access.Service {
	var store domain.SkillGrantStore
	if pool != nil {
		pg := postgres.NewPgSkillGrantStore(pool)
		if pg.Ready(ctx) {
			store = pg
		} else {
			slog.Warn("skill access: grant tables missing (run `orchestrator migrate up`); profile agents get only their profile skills")
		}
	}
	known := func(name string) bool { _, ok := reg.Get(name); return ok }
	svc, err := access.New(store, profileSkillSets(agents), known, hub, access.Config{
		RequestTimeout: time.Duration(cfg.AccessRequestTimeoutSeconds) * time.Second,
		AutoApprove:    cfg.AccessAutoApprove,
	}, nil)
	if err != nil {
		slog.Error("skill access: not built; profile agents are unbounded", "err", err)
		return nil
	}
	if cfg.AccessAutoApprove {
		slog.Warn("skill access: execution.skills.access_auto_approve is on — every agent skill request is granted without an operator")
	}
	slog.Info("skill access wired", "profile_agents", len(agents), "durable_grants", store != nil,
		"request_timeout_s", cfg.AccessRequestTimeoutSeconds, "auto_approve", cfg.AccessAutoApprove)
	return svc
}
