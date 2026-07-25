package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// PgSessionStore is the Postgres-backed session repository, run store and checkpoint store
// (session Phase 4). One type because the three share a lifetime and a cascade: purging a
// session must reclaim its runs and their checkpoints, and that is one statement here.
//
// Schema is owned by migrations/0004_sessions_runs.sql, never by this file (PLAT-02 /
// ADR-0064), so the DDL cannot drift from a second copy embedded in Go.
type PgSessionStore struct {
	pool *pgxpool.Pool
}

var (
	_ domain.RunStore        = (*PgSessionStore)(nil)
	_ domain.CheckpointStore = (*PgSessionStore)(nil)
)

const sessionCols = `id, parent_id, goal, status, summary, caller_scope, created_at, updated_at, completed_at, conversation_id, origin_message_id`

// NewPgSessionStore returns the store, failing with an actionable error when the schema has
// not been migrated.
func NewPgSessionStore(ctx context.Context, pool *pgxpool.Pool) (*PgSessionStore, error) {
	var present bool
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass('public.sessions') IS NOT NULL`).Scan(&present); err != nil {
		return nil, fmt.Errorf("check sessions schema: %w", err)
	}
	if !present {
		return nil, errors.New("sessions table is missing: run `cambrian migrate up` " +
			"(or leave storage.auto_migrate at its default of true). Schema is owned by " +
			"migration 0004_sessions_runs.sql")
	}
	// 0005 adds the conversation link. Checked separately so a half-migrated database says
	// which migration is missing rather than failing later on an unknown column.
	var linked bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_name = 'sessions' AND column_name = 'conversation_id')`).Scan(&linked); err != nil {
		return nil, fmt.Errorf("check sessions.conversation_id: %w", err)
	}
	if !linked {
		return nil, errors.New("sessions.conversation_id is missing: run `cambrian migrate up` " +
			"— schema is owned by migration 0005_session_conversation_link.sql")
	}
	return &PgSessionStore{pool: pool}, nil
}

// ── session.SessionRepository ────────────────────────────────────────────────

// SaveSession upserts a session.
func (s *PgSessionStore) SaveSession(ctx context.Context, ses domain.Session) error {
	scope, err := json.Marshal(ses.CallerScope)
	if err != nil {
		return fmt.Errorf("marshal caller_scope: %w", err)
	}
	var completed *time.Time
	if !ses.CompletedAt.IsZero() {
		c := ses.CompletedAt
		completed = &c
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO sessions (`+sessionCols+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (id) DO UPDATE SET
			conversation_id = EXCLUDED.conversation_id,
			origin_message_id = EXCLUDED.origin_message_id,
			parent_id = EXCLUDED.parent_id,
			goal = EXCLUDED.goal,
			status = EXCLUDED.status,
			summary = EXCLUDED.summary,
			caller_scope = EXCLUDED.caller_scope,
			updated_at = EXCLUDED.updated_at,
			completed_at = EXCLUDED.completed_at`,
		string(ses.ID), string(ses.ParentID), ses.Goal, string(ses.Status), ses.Summary,
		scope, ses.CreatedAt, ses.UpdatedAt, completed, ses.ConversationID, ses.OriginMessageID)
	if err != nil {
		return fmt.Errorf("save session: %w", err)
	}
	return nil
}

// GetSession returns a session, or (nil, nil) when unknown.
func (s *PgSessionStore) GetSession(ctx context.Context, id domain.SessionID) (*domain.Session, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+sessionCols+` FROM sessions WHERE id = $1`, string(id))
	ses, err := scanSession(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}
	return ses, nil
}

// ListSessions returns sessions with the given status; an empty status returns all.
func (s *PgSessionStore) ListSessions(ctx context.Context, status domain.SessionStatus) ([]domain.Session, error) {
	q := `SELECT ` + sessionCols + ` FROM sessions`
	args := []any{}
	if status != "" {
		q += ` WHERE status = $1`
		args = append(args, string(status))
	}
	q += ` ORDER BY updated_at DESC`

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	var out []domain.Session
	for rows.Next() {
		ses, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		out = append(out, *ses)
	}
	return out, rows.Err()
}

type scannable interface{ Scan(dest ...any) error }

func scanSession(row scannable) (*domain.Session, error) {
	var (
		ses       domain.Session
		id        string
		parentID  string
		status    string
		scope     []byte
		completed *time.Time
	)
	if err := row.Scan(&id, &parentID, &ses.Goal, &status, &ses.Summary,
		&scope, &ses.CreatedAt, &ses.UpdatedAt, &completed,
		&ses.ConversationID, &ses.OriginMessageID); err != nil {
		return nil, err
	}
	ses.ID = domain.SessionID(id)
	ses.ParentID = domain.SessionID(parentID)
	ses.Status = domain.SessionStatus(status)
	if len(scope) > 0 {
		// A malformed scope must not silently widen access: fail the read instead.
		if err := json.Unmarshal(scope, &ses.CallerScope); err != nil {
			return nil, fmt.Errorf("unmarshal caller_scope for session %s: %w", id, err)
		}
	}
	if completed != nil {
		ses.CompletedAt = *completed
	}
	return &ses, nil
}

// ListSessionsForConversation answers the ADR-0084 D2 question: what did this conversation
// set in motion? Newest first, served by the partial index on (conversation_id, created_at).
func (s *PgSessionStore) ListSessionsForConversation(ctx context.Context, conversationID string) ([]domain.Session, error) {
	if conversationID == "" {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+sessionCols+` FROM sessions WHERE conversation_id = $1 ORDER BY created_at DESC`,
		conversationID)
	if err != nil {
		return nil, fmt.Errorf("list sessions for conversation: %w", err)
	}
	defer rows.Close()

	var out []domain.Session
	for rows.Next() {
		ses, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		out = append(out, *ses)
	}
	return out, rows.Err()
}

// ── domain.RunStore ──────────────────────────────────────────────────────────

// SaveRun upserts a run, including the plan it executed.
func (s *PgSessionStore) SaveRun(run domain.Run) error {
	ctx := context.Background()
	var plan []byte
	if run.Plan != nil {
		b, err := json.Marshal(run.Plan)
		if err != nil {
			return fmt.Errorf("marshal plan: %w", err)
		}
		plan = b
	}
	var ended *time.Time
	if !run.EndedAt.IsZero() {
		e := run.EndedAt
		ended = &e
	}
	// session_id is nullable: a run may outlive a purged session, and a run started
	// outside any session (scout/bypass dispatch) has none at all.
	var session *string
	if run.SessionID != "" {
		v := string(run.SessionID)
		session = &v
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO runs (id, session_id, plan_id, subject, status, plan, started_at, ended_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (id) DO UPDATE SET
			status = EXCLUDED.status,
			subject = EXCLUDED.subject,
			plan = EXCLUDED.plan,
			ended_at = EXCLUDED.ended_at`,
		string(run.ID), session, run.PlanID, run.Subject, string(run.Status), plan,
		run.StartedAt, ended)
	if err != nil {
		return fmt.Errorf("save run: %w", err)
	}
	return nil
}

// GetRun returns a run, or (nil, nil) when unknown.
func (s *PgSessionStore) GetRun(runID domain.RunID) (*domain.Run, error) {
	row := s.pool.QueryRow(context.Background(), `
		SELECT id, COALESCE(session_id,''), plan_id, subject, status, plan, started_at, ended_at
		FROM runs WHERE id = $1`, string(runID))
	run, err := scanRun(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get run: %w", err)
	}
	return run, nil
}

// ListRunsForSession returns a session's runs, oldest first.
func (s *PgSessionStore) ListRunsForSession(sessionID domain.SessionID) ([]domain.Run, error) {
	rows, err := s.pool.Query(context.Background(), `
		SELECT id, COALESCE(session_id,''), plan_id, subject, status, plan, started_at, ended_at
		FROM runs WHERE session_id = $1 ORDER BY started_at`, string(sessionID))
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()

	var out []domain.Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		out = append(out, *run)
	}
	return out, rows.Err()
}

func scanRun(row scannable) (*domain.Run, error) {
	var (
		run     domain.Run
		id      string
		session string
		status  string
		plan    []byte
		ended   *time.Time
	)
	if err := row.Scan(&id, &session, &run.PlanID, &run.Subject, &status, &plan,
		&run.StartedAt, &ended); err != nil {
		return nil, err
	}
	run.ID = domain.RunID(id)
	run.SessionID = domain.SessionID(session)
	run.Status = domain.RunStatus(status)
	if len(plan) > 0 {
		var p domain.ExecutionPlan
		if err := json.Unmarshal(plan, &p); err != nil {
			return nil, fmt.Errorf("unmarshal plan for run %s: %w", id, err)
		}
		run.Plan = &p
	}
	if ended != nil {
		run.EndedAt = *ended
	}
	return &run, nil
}

// ── domain.CheckpointStore ───────────────────────────────────────────────────

// SaveCheckpoint upserts one step's context under its run.
func (s *PgSessionStore) SaveCheckpoint(runID domain.RunID, stepIndex int, cpCtx map[string]string) error {
	blob, err := json.Marshal(cpCtx)
	if err != nil {
		return fmt.Errorf("marshal checkpoint: %w", err)
	}
	_, err = s.pool.Exec(context.Background(), `
		INSERT INTO run_checkpoints (run_id, step_index, context, created_at)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (run_id, step_index) DO UPDATE SET
			context = EXCLUDED.context,
			created_at = EXCLUDED.created_at`,
		string(runID), stepIndex, blob, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("save checkpoint: %w", err)
	}
	return nil
}

// LoadCheckpoint returns one step's context, or (nil, nil) when absent.
func (s *PgSessionStore) LoadCheckpoint(runID domain.RunID, stepIndex int) (map[string]string, error) {
	var blob []byte
	err := s.pool.QueryRow(context.Background(),
		`SELECT context FROM run_checkpoints WHERE run_id = $1 AND step_index = $2`,
		string(runID), stepIndex).Scan(&blob)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load checkpoint: %w", err)
	}
	out := map[string]string{}
	if err := json.Unmarshal(blob, &out); err != nil {
		return nil, fmt.Errorf("unmarshal checkpoint: %w", err)
	}
	return out, nil
}

// ListCheckpoints returns a run's checkpoints in ascending step order. step_index is an
// INTEGER column, so the ordering is numeric by construction rather than by key formatting.
func (s *PgSessionStore) ListCheckpoints(runID domain.RunID) ([]domain.CheckpointMeta, error) {
	rows, err := s.pool.Query(context.Background(), `
		SELECT c.run_id, COALESCE(r.session_id,''), r.plan_id, c.step_index, c.created_at
		FROM run_checkpoints c
		JOIN runs r ON r.id = c.run_id
		WHERE c.run_id = $1
		ORDER BY c.step_index`, string(runID))
	if err != nil {
		return nil, fmt.Errorf("list checkpoints: %w", err)
	}
	defer rows.Close()

	var out []domain.CheckpointMeta
	for rows.Next() {
		var (
			m       domain.CheckpointMeta
			runStr  string
			session string
		)
		if err := rows.Scan(&runStr, &session, &m.PlanID, &m.StepIndex, &m.Timestamp); err != nil {
			return nil, fmt.Errorf("scan checkpoint: %w", err)
		}
		m.RunID = domain.RunID(runStr)
		m.SessionID = domain.SessionID(session)
		out = append(out, m)
	}
	return out, rows.Err()
}

// ── retention ────────────────────────────────────────────────────────────────

// PurgeCompletedBefore deletes completed sessions finished before cutoff and returns how
// many rows went. Runs and run_checkpoints follow by ON DELETE CASCADE — which is the point
// of moving this out of bbolt: there, reclaiming a session meant three hand-written sweeps
// that could drift apart, so in practice nothing was ever reclaimed at all.
//
// Only COMPLETED sessions are eligible. Active, paused and dormant sessions are live state
// regardless of age; a dormant session is explicitly resumable.
func (s *PgSessionStore) PurgeCompletedBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM sessions WHERE status = $1 AND completed_at IS NOT NULL AND completed_at < $2`,
		string(domain.SessionCompleted), cutoff)
	if err != nil {
		return 0, fmt.Errorf("purge sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

// PurgeOrphanRunsBefore deletes runs that have no session (their session was purged before
// the cascade existed, or they were started outside one) and ended before cutoff.
func (s *PgSessionStore) PurgeOrphanRunsBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM runs WHERE session_id IS NULL AND started_at < $1`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("purge orphan runs: %w", err)
	}
	return tag.RowsAffected(), nil
}
