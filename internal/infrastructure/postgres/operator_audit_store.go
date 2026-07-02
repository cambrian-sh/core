package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// PgAuditStore is the durable Postgres-backed operator_audit store (ADR-0047
// D15/0047-24), implementing domain.AuditStore. The UNIQUE command_id column is
// the idempotency dedup. Pass the pool from PgVectorAdapter.Pool() to reuse the
// shared connection.
type PgAuditStore struct {
	pool *pgxpool.Pool
}

const createOperatorAuditTable = `
CREATE TABLE IF NOT EXISTS operator_audit (
	id           TEXT PRIMARY KEY,
	command_id   TEXT NOT NULL UNIQUE,
	ts           TIMESTAMPTZ NOT NULL,
	actor        TEXT NOT NULL,
	role         TEXT NOT NULL,
	action_type  TEXT NOT NULL,
	target_type  TEXT NOT NULL,
	target_id    TEXT NOT NULL,
	before_json  TEXT NOT NULL,
	after_json   TEXT NOT NULL,
	reason       TEXT NOT NULL,
	result       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_operator_audit_ts ON operator_audit (ts DESC);
CREATE INDEX IF NOT EXISTS idx_operator_audit_actor ON operator_audit (actor);
CREATE INDEX IF NOT EXISTS idx_operator_audit_target ON operator_audit (target_type, target_id);
`

var _ domain.AuditStore = (*PgAuditStore)(nil)

// NewPgAuditStore ensures the operator_audit table exists and returns the store.
func NewPgAuditStore(ctx context.Context, pool *pgxpool.Pool) (*PgAuditStore, error) {
	if _, err := pool.Exec(ctx, createOperatorAuditTable); err != nil {
		return nil, fmt.Errorf("create operator_audit table: %w", err)
	}
	return &PgAuditStore{pool: pool}, nil
}

// Record inserts the entry. The UNIQUE command_id makes a retried command a
// no-op: ON CONFLICT DO NOTHING; deduped is true when no row was inserted.
func (s *PgAuditStore) Record(ctx context.Context, e domain.AuditEntry) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO operator_audit
			(id, command_id, ts, actor, role, action_type, target_type, target_id, before_json, after_json, reason, result)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (command_id) DO NOTHING`,
		e.ID, e.CommandID, e.At, e.Actor, e.Role, e.ActionType, e.TargetType, e.TargetID, e.Before, e.After, e.Reason, e.Result,
	)
	if err != nil {
		return false, fmt.Errorf("insert operator_audit: %w", err)
	}
	return tag.RowsAffected() == 0, nil // 0 rows ⇒ command_id already present ⇒ deduped
}

// Query returns matching entries, most recent first.
func (s *PgAuditStore) Query(ctx context.Context, f domain.AuditFilter) ([]domain.AuditEntry, error) {
	var conds []string
	var args []any
	add := func(col, val string) {
		if val != "" {
			args = append(args, val)
			conds = append(conds, fmt.Sprintf("%s = $%d", col, len(args)))
		}
	}
	add("actor", f.Actor)
	add("target_type", f.TargetType)
	add("target_id", f.TargetID)
	add("action_type", f.ActionType)

	q := `SELECT id, command_id, ts, actor, role, action_type, target_type, target_id, before_json, after_json, reason, result FROM operator_audit`
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY ts DESC"
	if f.Limit > 0 {
		args = append(args, f.Limit)
		q += fmt.Sprintf(" LIMIT $%d", len(args))
	}

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query operator_audit: %w", err)
	}
	defer rows.Close()

	var out []domain.AuditEntry
	for rows.Next() {
		var e domain.AuditEntry
		if err := rows.Scan(&e.ID, &e.CommandID, &e.At, &e.Actor, &e.Role, &e.ActionType,
			&e.TargetType, &e.TargetID, &e.Before, &e.After, &e.Reason, &e.Result); err != nil {
			return nil, fmt.Errorf("scan operator_audit: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
