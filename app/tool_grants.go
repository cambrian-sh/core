package app

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
)

// buildToolGrants assembles the kernel's tool grants view (ENG-1116): operator
// grants in the durable 0052 tool_grants table when the kernel has a database
// that carries it, merged over the in-memory profile-allowlist layer the profile
// grant sync writes. With no pool, or an unmigrated database (migrations mode
// off / refuse_pending), operator grants stay in memory with a loud warning —
// the same fail-soft posture as the operator audit and action record stores; the
// kernel still boots, and a grant check never allows on an error either way.
func buildToolGrants(ctx context.Context, pool *pgxpool.Pool) (*domain.LayeredGrantsStore, error) {
	var operatorLayer domain.ToolGrantStore
	switch {
	case pool == nil:
		slog.Warn("tool grants: no database; operator tool grants stay in memory and do not survive a restart")
		operatorLayer = domain.NewInMemoryGrantsStore()
	default:
		pg := postgres.NewPgToolGrantStore(pool)
		if pg.Ready(ctx) {
			operatorLayer = pg
		} else {
			slog.Warn("tool grants: tool_grants table missing (migration 0052 not applied); operator tool grants stay in memory and do not survive a restart")
			operatorLayer = domain.NewInMemoryGrantsStore()
		}
	}
	grants, err := domain.NewLayeredGrantsStore(operatorLayer)
	if err != nil {
		return nil, fmt.Errorf("tool grants: %w", err)
	}
	return grants, nil
}
