package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ENG-128 (owner decision 2026-11-15): bounded retention deletes for the tables
// the retention worker (internal/retention) prunes. Each call removes at most
// limit rows, oldest first, so a backlog drains over several passes instead of
// one long-locking DELETE. Evidence and knowledge tables are deliberately absent:
// they are the record and are deleted only through the erasure path.

// PruneOperatorAuditBefore deletes up to limit operator_audit rows with ts before
// `before` (default policy: 365 days).
func (s *PgAuditStore) PruneOperatorAuditBefore(ctx context.Context, before time.Time, limit int) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM operator_audit WHERE id IN (
			SELECT id FROM operator_audit WHERE ts < $1 ORDER BY ts LIMIT $2)`, before, limit)
	if err != nil {
		return 0, fmt.Errorf("prune operator_audit: %w", err)
	}
	return tag.RowsAffected(), nil
}

// PruneChunkTripletsBefore deletes up to limit chunk_triplets rows extracted
// before `before`. Triplets are derived from chunks and can be re-extracted.
func PruneChunkTripletsBefore(ctx context.Context, pool *pgxpool.Pool, before time.Time, limit int) (int64, error) {
	tag, err := pool.Exec(ctx, `
		DELETE FROM chunk_triplets WHERE ctid IN (
			SELECT ctid FROM chunk_triplets WHERE extracted_at < $1 ORDER BY extracted_at LIMIT $2)`, before, limit)
	if err != nil {
		return 0, fmt.Errorf("prune chunk_triplets: %w", err)
	}
	return tag.RowsAffected(), nil
}

// PruneExperiencesBefore deletes up to limit experiences created before
// `before`. Their chunks go with them through the FK cascade (ADR-0095 D6:
// forgetting an episode is one delete). experiences.created_at is TIMESTAMP
// without time zone written by NOW() in the session zone, so the bound is passed
// as UTC wall time.
func PruneExperiencesBefore(ctx context.Context, pool *pgxpool.Pool, before time.Time, limit int) (int64, error) {
	tag, err := pool.Exec(ctx, `
		DELETE FROM experiences WHERE id IN (
			SELECT id FROM experiences WHERE created_at < $1 ORDER BY created_at LIMIT $2)`,
		before.UTC().Format("2006-01-02 15:04:05.999999"), limit)
	if err != nil {
		return 0, fmt.Errorf("prune experiences: %w", err)
	}
	return tag.RowsAffected(), nil
}
