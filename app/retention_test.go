package app

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
)

// ENG-128: execution.retention reaches the worker — defaults run only the audit
// rule; a derived-table TTL turns its rule on; no Postgres means no rules.
func TestBuildRetentionWorker_ReadsConfig(t *testing.T) {
	// pgxpool.New does not connect until used, so this needs no database.
	pool, err := pgxpool.New(context.Background(), "postgres://x@127.0.0.1:1/none")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	audit := &postgres.PgAuditStore{}

	w := buildRetentionWorker(config.DefaultRetentionConfig(), audit, pool)
	if got := w.Active(); !slices.Equal(got, []string{"operator_audit"}) {
		t.Fatalf("default active rules = %v, want only operator_audit", got)
	}

	rc := config.DefaultRetentionConfig()
	rc.ChunkTripletsDays, rc.ExperiencesDays, rc.OperatorAuditDays = 30, 90, 0
	if got := buildRetentionWorker(rc, audit, pool).Active(); !slices.Equal(got, []string{"chunk_triplets", "experiences"}) {
		t.Fatalf("active rules = %v", got)
	}

	if got := buildRetentionWorker(config.DefaultRetentionConfig(), nil, nil).Active(); len(got) != 0 {
		t.Fatalf("without Postgres no rule may run, got %v", got)
	}
}
