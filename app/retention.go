package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/retention"
)

// buildRetentionWorker turns execution.retention into the retention worker
// (ENG-128, owner decision 2026-11-15): operator audit kept 365 days by default,
// chunk_triplets and experiences opt-in, evidence and knowledge never TTL'd.
//
// audit is nil when the durable audit store is unavailable (in-memory fallback);
// pool is nil without Postgres. Rules whose table is unavailable are omitted.
func buildRetentionWorker(rc config.RetentionConfig, audit *postgres.PgAuditStore, pool *pgxpool.Pool) *retention.Worker {
	days := func(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }
	var rules []retention.Rule
	if audit != nil {
		rules = append(rules, retention.Rule{Name: "operator_audit", Keep: days(rc.OperatorAuditDays), Purge: audit.PruneOperatorAuditBefore})
	}
	if pool != nil {
		rules = append(rules,
			retention.Rule{Name: "chunk_triplets", Keep: days(rc.ChunkTripletsDays),
				Purge: func(ctx context.Context, before time.Time, limit int) (int64, error) {
					return postgres.PruneChunkTripletsBefore(ctx, pool, before, limit)
				}},
			retention.Rule{Name: "experiences", Keep: days(rc.ExperiencesDays),
				Purge: func(ctx context.Context, before time.Time, limit int) (int64, error) {
					return postgres.PruneExperiencesBefore(ctx, pool, before, limit)
				}},
		)
	}
	w := retention.New(rules, time.Duration(rc.SweepIntervalSeconds)*time.Second, rc.BatchSize, slog.Default())
	slog.Info("ENG-128: retention worker configured",
		"active", w.Active(), "operator_audit_days", rc.OperatorAuditDays,
		"chunk_triplets_days", rc.ChunkTripletsDays, "experiences_days", rc.ExperiencesDays,
		"sweep_interval_seconds", rc.SweepIntervalSeconds, "batch_size", rc.BatchSize)
	return w
}
