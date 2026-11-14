package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// PgActionRecordStore is the durable domain.ActionRecordStore over the
// 0046_action_records table (ADR-0154). Schema comes from the migration runner;
// this adapter never creates tables. Record is INSERT … ON CONFLICT DO NOTHING
// on the deterministic key, so a retried write is idempotent; Advance is a
// compare-and-set on the state it read, so two writers racing on one record
// cannot both move it.
type PgActionRecordStore struct {
	pool *pgxpool.Pool
}

var _ domain.ActionRecordStore = (*PgActionRecordStore)(nil)

// NewPgActionRecordStore wraps the shared pool.
func NewPgActionRecordStore(pool *pgxpool.Pool) *PgActionRecordStore {
	return &PgActionRecordStore{pool: pool}
}

// Ready reports whether the 0046 table exists.
func (s *PgActionRecordStore) Ready(ctx context.Context) bool {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name = 'action_records'`).Scan(&n)
	return err == nil && n == 1
}

const actionRecordCols = `key, tool, run_id, step_index, principal, session_id, args_hash, state, result, error, created_at, updated_at`

func scanActionRecord(row pgx.Row) (domain.DurableAction, error) {
	var r domain.DurableAction
	var run, session, state string
	var result []byte
	if err := row.Scan(&r.Key, &r.Tool, &run, &r.StepIndex, &r.Principal, &session, &r.ArgsHash,
		&state, &result, &r.Error, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return domain.DurableAction{}, err
	}
	r.RunID, r.SessionID, r.State = domain.RunID(run), domain.SessionID(session), domain.ActionState(state)
	if len(result) > 0 {
		var resp domain.ToolCallResponse
		if err := json.Unmarshal(result, &resp); err != nil {
			return domain.DurableAction{}, fmt.Errorf("action record %s: decode result: %w", r.Key, err)
		}
		r.Result = &resp
	}
	return r, nil
}

func encodeActionResult(r *domain.ToolCallResponse) ([]byte, error) {
	if r == nil {
		return nil, nil
	}
	return json.Marshal(r)
}

// Record inserts rec under its key, or returns the existing row unchanged.
func (s *PgActionRecordStore) Record(ctx context.Context, rec domain.DurableAction) (domain.DurableAction, bool, error) {
	if rec.Key == "" {
		return domain.DurableAction{}, false, errors.New("action record: key is required")
	}
	if rec.State == "" {
		rec.State = domain.ActionStateProposed
	}
	if !rec.State.Valid() {
		return domain.DurableAction{}, false, fmt.Errorf("action record: unknown state %q", rec.State)
	}
	result, err := encodeActionResult(rec.Result)
	if err != nil {
		return domain.DurableAction{}, false, fmt.Errorf("action record %s: encode result: %w", rec.Key, err)
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO action_records
		(key, tool, run_id, step_index, principal, session_id, args_hash, state, result, error)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (key) DO NOTHING`,
		rec.Key, rec.Tool, string(rec.RunID), rec.StepIndex, rec.Principal, string(rec.SessionID),
		rec.ArgsHash, string(rec.State), result, rec.Error)
	if err != nil {
		return domain.DurableAction{}, false, fmt.Errorf("action record %s: insert: %w", rec.Key, err)
	}
	stored, err := s.Get(ctx, rec.Key)
	if err != nil {
		return domain.DurableAction{}, false, err
	}
	return stored, tag.RowsAffected() == 1, nil
}

// Advance moves the record when the lifecycle allows it (compare-and-set on the
// state read).
func (s *PgActionRecordStore) Advance(ctx context.Context, key string, to domain.ActionState, u domain.ActionUpdate) (domain.DurableAction, error) {
	cur, err := s.Get(ctx, key)
	if err != nil {
		return domain.DurableAction{}, err
	}
	if !cur.State.CanAdvanceTo(to) {
		return cur, fmt.Errorf("%w: %s → %s", domain.ErrActionTransition, cur.State, to)
	}
	result, err := encodeActionResult(u.Result)
	if err != nil {
		return domain.DurableAction{}, fmt.Errorf("action record %s: encode result: %w", key, err)
	}
	tag, err := s.pool.Exec(ctx, `UPDATE action_records
		SET state = $3, result = COALESCE($4, result), error = $5, updated_at = now()
		WHERE key = $1 AND state = $2`,
		key, string(cur.State), string(to), result, u.Error)
	if err != nil {
		return domain.DurableAction{}, fmt.Errorf("action record %s: advance: %w", key, err)
	}
	if tag.RowsAffected() == 0 {
		// Someone moved it between the read and the write: report what is there.
		now, gerr := s.Get(ctx, key)
		if gerr != nil {
			return domain.DurableAction{}, gerr
		}
		return now, fmt.Errorf("%w: %s changed to %s concurrently", domain.ErrActionTransition, key, now.State)
	}
	return s.Get(ctx, key)
}

// Get returns the record by key.
func (s *PgActionRecordStore) Get(ctx context.Context, key string) (domain.DurableAction, error) {
	r, err := scanActionRecord(s.pool.QueryRow(ctx, `SELECT `+actionRecordCols+` FROM action_records WHERE key = $1`, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.DurableAction{}, domain.ErrActionRecordNotFound
	}
	if err != nil {
		return domain.DurableAction{}, fmt.Errorf("action record %s: get: %w", key, err)
	}
	return r, nil
}
