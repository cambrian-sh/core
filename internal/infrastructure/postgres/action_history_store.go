package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// PgActionHistoryStore persists the done entries of the conversation action
// history (domain.ActionHistoryStore; table action_history, migration 0066,
// ENG-784), so the approver's duplicate notice still names a conversation's
// earlier writes after a kernel restart. Rows older than the history's window
// are deleted on every write and never read: the arguments held here are bounded
// by execution.tools.duplicate_notice_window_minutes.
type PgActionHistoryStore struct{ pool *pgxpool.Pool }

// NewPgActionHistoryStore returns the store over pool.
func NewPgActionHistoryStore(pool *pgxpool.Pool) *PgActionHistoryStore {
	return &PgActionHistoryStore{pool: pool}
}

var _ domain.ActionHistoryStore = (*PgActionHistoryStore)(nil)

// Ready reports whether the action_history table exists (migration 0066).
func (s *PgActionHistoryStore) Ready(ctx context.Context) bool {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT to_regclass('action_history') IS NOT NULL`).Scan(&ok)
	return err == nil && ok
}

// RecordDone upserts rec under key and deletes every row done before keepAfter.
func (s *PgActionHistoryStore) RecordDone(ctx context.Context, scope string, rec domain.ActionRecord, key string, keepAfter time.Time) error {
	ids, err := json.Marshal(nonNilStrings(rec.IDs))
	if err != nil {
		return fmt.Errorf("action history: ids: %w", err)
	}
	args := rec.Args
	if args == nil {
		args = []byte{}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("action history: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit
	if _, err := tx.Exec(ctx, `INSERT INTO action_history (key, scope, tool, args, approval_id, ids, done_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (key) DO UPDATE SET done_at = EXCLUDED.done_at`,
		key, scope, rec.Tool, args, rec.ApprovalID, ids, rec.At.UTC()); err != nil {
		return fmt.Errorf("action history: record: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM action_history WHERE done_at < $1`, keepAfter.UTC()); err != nil {
		return fmt.Errorf("action history: prune: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("action history: commit: %w", err)
	}
	return nil
}

// LoadDone returns scope's rows done after since, oldest first, at most limit
// (the most recent ones).
func (s *PgActionHistoryStore) LoadDone(ctx context.Context, scope string, since time.Time, limit int) ([]domain.ActionRecord, error) {
	rows, err := s.pool.Query(ctx, `SELECT tool, args, approval_id, ids, done_at FROM (
			SELECT tool, args, approval_id, ids, done_at FROM action_history
			WHERE scope = $1 AND done_at > $2 ORDER BY done_at DESC LIMIT $3
		) recent ORDER BY done_at`, scope, since.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("action history: load: %w", err)
	}
	defer rows.Close()
	var out []domain.ActionRecord
	for rows.Next() {
		var r domain.ActionRecord
		var ids []byte
		if err := rows.Scan(&r.Tool, &r.Args, &r.ApprovalID, &ids, &r.At); err != nil {
			return nil, fmt.Errorf("action history: scan: %w", err)
		}
		if err := json.Unmarshal(ids, &r.IDs); err != nil {
			return nil, fmt.Errorf("action history: ids: %w", err)
		}
		r.State = domain.ActionDone
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("action history: load: %w", err)
	}
	return out, nil
}

func nonNilStrings(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}
