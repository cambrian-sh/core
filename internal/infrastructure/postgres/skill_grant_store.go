package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// PgSkillGrantStore is the durable domain.SkillGrantStore over the 0031_skill_grants
// tables (decisions/skill-access-grants.md). Schema comes from the migration runner;
// this adapter never creates tables. Every changing write appends its audit event
// in the same transaction, serialised per (agent, skill) by an advisory lock;
// skill_grant_events is append-only (a trigger refuses UPDATE and DELETE).
type PgSkillGrantStore struct {
	pool *pgxpool.Pool
}

var _ domain.SkillGrantStore = (*PgSkillGrantStore)(nil)

// NewPgSkillGrantStore wraps the shared pool.
func NewPgSkillGrantStore(pool *pgxpool.Pool) *PgSkillGrantStore {
	return &PgSkillGrantStore{pool: pool}
}

// Ready reports whether the 0031 tables exist.
func (s *PgSkillGrantStore) Ready(ctx context.Context) bool {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name IN ('skill_grants','skill_access_requests','skill_grant_events')`).Scan(&n)
	return err == nil && n == 3
}

// pairTx runs fn in one transaction holding the (agent, skill) advisory lock.
func (s *PgSkillGrantStore) pairTx(ctx context.Context, agentID, skill string, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("skill grant: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('skill-grant:' || $1 || chr(31) || $2, 0))`, agentID, skill); err != nil {
		return fmt.Errorf("skill grant: lock: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("skill grant: commit: %w", err)
	}
	return nil
}

// appendEvent writes the pair's next audit row inside tx.
func appendEvent(ctx context.Context, tx pgx.Tx, e domain.SkillGrantEvent) error {
	var head int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(seq), 0) FROM skill_grant_events WHERE agent_id = $1 AND skill_name = $2`,
		e.AgentID, e.SkillName).Scan(&head); err != nil {
		return fmt.Errorf("skill grant event: head: %w", err)
	}
	e.Seq = head + 1
	e.ID = domain.SkillGrantEventID(e.AgentID, e.SkillName, e.Seq)
	if _, err := tx.Exec(ctx, `INSERT INTO skill_grant_events (id, agent_id, skill_name, seq, action, source, actor, reason, run_id, at, command_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		e.ID, e.AgentID, e.SkillName, e.Seq, e.Action, e.Source, e.Actor, e.Reason, e.RunID, e.At.UTC(), e.CommandID); err != nil {
		return fmt.Errorf("skill grant event: insert: %w", err)
	}
	return nil
}

// Grant implements domain.SkillGrantStore.
func (s *PgSkillGrantStore) Grant(ctx context.Context, g domain.SkillGrant, commandID string) (bool, error) {
	changed := false
	err := s.pairTx(ctx, g.AgentID, g.SkillName, func(tx pgx.Tx) error {
		var revoked *time.Time
		err := tx.QueryRow(ctx, `SELECT revoked_at FROM skill_grants WHERE agent_id = $1 AND skill_name = $2`,
			g.AgentID, g.SkillName).Scan(&revoked)
		exists := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("skill grant: read: %w", err)
		}
		if exists && revoked == nil {
			return nil // already active: no-op, no audit row
		}
		if _, err := tx.Exec(ctx, `INSERT INTO skill_grants (id, agent_id, skill_name, source, granted_by, granted_at, reason)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (agent_id, skill_name) DO UPDATE SET source = EXCLUDED.source, granted_by = EXCLUDED.granted_by,
				granted_at = EXCLUDED.granted_at, reason = EXCLUDED.reason,
				revoked_at = NULL, revoked_by = '', revoke_reason = ''`,
			domain.SkillGrantID(g.AgentID, g.SkillName), g.AgentID, g.SkillName, g.Source, g.GrantedBy, g.GrantedAt.UTC(), g.Reason); err != nil {
			return fmt.Errorf("skill grant: upsert: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE skill_access_requests SET state = 'granted', decided_at = $3, decided_by = $4
			WHERE agent_id = $1 AND skill_name = $2 AND state <> 'granted'`,
			g.AgentID, g.SkillName, g.GrantedAt.UTC(), g.GrantedBy); err != nil {
			return fmt.Errorf("skill grant: resolve request: %w", err)
		}
		changed = true
		return appendEvent(ctx, tx, domain.SkillGrantEvent{
			AgentID: g.AgentID, SkillName: g.SkillName, Action: domain.SkillGrantActionGranted, Source: g.Source,
			Actor: g.GrantedBy, Reason: g.Reason, At: g.GrantedAt, CommandID: commandID,
		})
	})
	return changed, err
}

// Revoke implements domain.SkillGrantStore.
func (s *PgSkillGrantStore) Revoke(ctx context.Context, agentID, skill, by, reason, commandID string, at time.Time) error {
	return s.pairTx(ctx, agentID, skill, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE skill_grants SET revoked_at = $3, revoked_by = $4, revoke_reason = $5
			WHERE agent_id = $1 AND skill_name = $2 AND revoked_at IS NULL`, agentID, skill, at.UTC(), by, reason)
		if err != nil {
			return fmt.Errorf("skill grant: revoke: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrSkillGrantNotFound
		}
		return appendEvent(ctx, tx, domain.SkillGrantEvent{
			AgentID: agentID, SkillName: skill, Action: domain.SkillGrantActionRevoked,
			Actor: by, Reason: reason, At: at, CommandID: commandID,
		})
	})
}

const grantCols = `id, agent_id, skill_name, source, granted_by, granted_at, reason, revoked_at, revoked_by, revoke_reason`

func scanGrant(row pgx.Row) (domain.SkillGrant, error) {
	var g domain.SkillGrant
	var revoked *time.Time
	err := row.Scan(&g.ID, &g.AgentID, &g.SkillName, &g.Source, &g.GrantedBy, &g.GrantedAt, &g.Reason, &revoked, &g.RevokedBy, &g.RevokeReason)
	if revoked != nil {
		g.RevokedAt = *revoked
	}
	return g, err
}

func whereOf(conds []string) string {
	if len(conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

// ActiveGrants implements domain.SkillGrantStore.
func (s *PgSkillGrantStore) ActiveGrants(ctx context.Context, agentID string) ([]domain.SkillGrant, error) {
	return s.ListGrants(ctx, domain.SkillGrantFilter{AgentID: agentID})
}

// ListGrants implements domain.SkillGrantStore (by agent, then skill).
func (s *PgSkillGrantStore) ListGrants(ctx context.Context, f domain.SkillGrantFilter) ([]domain.SkillGrant, error) {
	var conds []string
	var args []any
	if f.AgentID != "" {
		args = append(args, f.AgentID)
		conds = append(conds, fmt.Sprintf("agent_id = $%d", len(args)))
	}
	if f.SkillName != "" {
		args = append(args, f.SkillName)
		conds = append(conds, fmt.Sprintf("skill_name = $%d", len(args)))
	}
	if !f.IncludeRevoked {
		conds = append(conds, "revoked_at IS NULL")
	}
	rows, err := s.pool.Query(ctx, `SELECT `+grantCols+` FROM skill_grants`+whereOf(conds)+` ORDER BY agent_id, skill_name`, args...)
	if err != nil {
		return nil, fmt.Errorf("list skill grants: %w", err)
	}
	defer rows.Close()
	var out []domain.SkillGrant
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, fmt.Errorf("scan skill grant: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// OpenRequest implements domain.SkillGrantStore.
func (s *PgSkillGrantStore) OpenRequest(ctx context.Context, r domain.SkillAccessRequest) error {
	return s.pairTx(ctx, r.AgentID, r.SkillName, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO skill_access_requests (id, agent_id, skill_name, run_id, rationale, state, requested_at)
			VALUES ($1,$2,$3,$4,$5,'pending',$6)
			ON CONFLICT (agent_id, skill_name) DO UPDATE SET run_id = EXCLUDED.run_id, rationale = EXCLUDED.rationale,
				state = 'pending', requested_at = EXCLUDED.requested_at, decided_at = NULL, decided_by = ''`,
			domain.SkillAccessRequestID(r.AgentID, r.SkillName), r.AgentID, r.SkillName, r.RunID, r.Rationale, r.RequestedAt.UTC()); err != nil {
			return fmt.Errorf("skill access request: upsert: %w", err)
		}
		return appendEvent(ctx, tx, domain.SkillGrantEvent{
			AgentID: r.AgentID, SkillName: r.SkillName, Action: domain.SkillGrantActionRequested,
			Actor: r.AgentID, Reason: r.Rationale, RunID: r.RunID, At: r.RequestedAt,
		})
	})
}

// DenyRequest implements domain.SkillGrantStore.
func (s *PgSkillGrantStore) DenyRequest(ctx context.Context, agentID, skill, by string, at time.Time) error {
	return s.pairTx(ctx, agentID, skill, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE skill_access_requests SET state = 'denied', decided_at = $3, decided_by = $4
			WHERE agent_id = $1 AND skill_name = $2`, agentID, skill, at.UTC(), by); err != nil {
			return fmt.Errorf("skill access request: deny: %w", err)
		}
		return appendEvent(ctx, tx, domain.SkillGrantEvent{
			AgentID: agentID, SkillName: skill, Action: domain.SkillGrantActionDenied, Actor: by, At: at,
		})
	})
}

// ListRequests implements domain.SkillGrantStore (newest first).
func (s *PgSkillGrantStore) ListRequests(ctx context.Context, f domain.SkillGrantFilter) ([]domain.SkillAccessRequest, error) {
	var conds []string
	var args []any
	for _, kv := range [][2]string{{"agent_id", f.AgentID}, {"skill_name", f.SkillName}, {"state", f.State}} {
		if kv[1] != "" {
			args = append(args, kv[1])
			conds = append(conds, fmt.Sprintf("%s = $%d", kv[0], len(args)))
		}
	}
	rows, err := s.pool.Query(ctx, `SELECT id, agent_id, skill_name, run_id, rationale, state, requested_at, decided_at, decided_by
		FROM skill_access_requests`+whereOf(conds)+` ORDER BY requested_at DESC, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list skill access requests: %w", err)
	}
	defer rows.Close()
	var out []domain.SkillAccessRequest
	for rows.Next() {
		var r domain.SkillAccessRequest
		var decided *time.Time
		if err := rows.Scan(&r.ID, &r.AgentID, &r.SkillName, &r.RunID, &r.Rationale, &r.State, &r.RequestedAt, &decided, &r.DecidedBy); err != nil {
			return nil, fmt.Errorf("scan skill access request: %w", err)
		}
		if decided != nil {
			r.DecidedAt = *decided
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

const eventCols = `id, agent_id, skill_name, seq, action, source, actor, reason, run_id, at, command_id`

func scanEvent(row pgx.Row) (domain.SkillGrantEvent, error) {
	var e domain.SkillGrantEvent
	err := row.Scan(&e.ID, &e.AgentID, &e.SkillName, &e.Seq, &e.Action, &e.Source, &e.Actor, &e.Reason, &e.RunID, &e.At, &e.CommandID)
	return e, err
}

// EventByCommand implements domain.SkillGrantStore.
func (s *PgSkillGrantStore) EventByCommand(ctx context.Context, commandID string) (domain.SkillGrantEvent, bool, error) {
	if commandID == "" {
		return domain.SkillGrantEvent{}, false, nil
	}
	e, err := scanEvent(s.pool.QueryRow(ctx, `SELECT `+eventCols+` FROM skill_grant_events WHERE command_id = $1`, commandID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.SkillGrantEvent{}, false, nil
	}
	if err != nil {
		return domain.SkillGrantEvent{}, false, fmt.Errorf("skill grant event by command: %w", err)
	}
	return e, true, nil
}

// ListEvents implements domain.SkillGrantStore (newest first).
func (s *PgSkillGrantStore) ListEvents(ctx context.Context, f domain.SkillGrantFilter, limit int) ([]domain.SkillGrantEvent, error) {
	var conds []string
	var args []any
	for _, kv := range [][2]string{{"agent_id", f.AgentID}, {"skill_name", f.SkillName}} {
		if kv[1] != "" {
			args = append(args, kv[1])
			conds = append(conds, fmt.Sprintf("%s = $%d", kv[0], len(args)))
		}
	}
	q := `SELECT ` + eventCols + ` FROM skill_grant_events` + whereOf(conds) + ` ORDER BY at DESC, id`
	q, args = limitOffset(q, args, limit, 0)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list skill grant events: %w", err)
	}
	defer rows.Close()
	var out []domain.SkillGrantEvent
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan skill grant event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
