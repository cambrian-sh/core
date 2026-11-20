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

// PgPythonPackageStore is the durable domain.PythonPackageStore over the
// 0057_python_package_approvals tables (ADR-0157 D7.8.4). Schema comes from the
// migration runner; this adapter never creates tables. Every changing write
// appends its audit event in the same transaction, serialised per record by an
// advisory lock; python_package_events is append-only (a trigger refuses UPDATE
// and DELETE).
type PgPythonPackageStore struct {
	pool *pgxpool.Pool
}

var _ domain.PythonPackageStore = (*PgPythonPackageStore)(nil)

// NewPgPythonPackageStore wraps the shared pool.
func NewPgPythonPackageStore(pool *pgxpool.Pool) *PgPythonPackageStore {
	return &PgPythonPackageStore{pool: pool}
}

// Ready reports whether the 0057 tables exist.
func (s *PgPythonPackageStore) Ready(ctx context.Context) bool {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name IN ('python_package_approvals','python_package_events')`).Scan(&n)
	return err == nil && n == 2
}

// recordTx runs fn in one transaction holding the record's advisory lock.
func (s *PgPythonPackageStore) recordTx(ctx context.Context, id string, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("python package: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('python-package:' || $1, 0))`, id); err != nil {
		return fmt.Errorf("python package: lock: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("python package: commit: %w", err)
	}
	return nil
}

func appendPythonPackageEvent(ctx context.Context, tx pgx.Tx, e domain.PythonPackageEvent) error {
	var head int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(seq), 0) FROM python_package_events WHERE record_id = $1`, e.ApprovalID).Scan(&head); err != nil {
		return fmt.Errorf("python package event: head: %w", err)
	}
	e.Seq = head + 1
	e.ID = domain.PythonPackageEventID(e.ApprovalID, e.Seq)
	if _, err := tx.Exec(ctx, `INSERT INTO python_package_events (id, record_id, seq, target, name, version, action, actor, detail, generation, at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		e.ID, e.ApprovalID, e.Seq, e.Target, e.Name, e.Version, e.Action, e.Actor, e.Detail, e.Generation, e.At.UTC()); err != nil {
		return fmt.Errorf("python package event: insert: %w", err)
	}
	return nil
}

const pyPkgCols = `id, target, name, version, extras, lock_sha256, lock, base_generation, state, approval_id,
	requested_by, requested_at, decided_by, decided_at, detail`

func scanPythonPackage(row pgx.Row) (domain.PythonPackageApproval, error) {
	var a domain.PythonPackageApproval
	var extras, lock []byte
	var decided *time.Time
	if err := row.Scan(&a.ID, &a.Target, &a.Name, &a.Version, &extras, &a.LockSHA256, &lock, &a.BaseGeneration, &a.State,
		&a.ApprovalID, &a.RequestedBy, &a.RequestedAt, &a.DecidedBy, &decided, &a.Detail); err != nil {
		return a, err
	}
	if decided != nil {
		a.DecidedAt = *decided
	}
	if err := json.Unmarshal(extras, &a.Extras); err != nil {
		return a, fmt.Errorf("python package %s: extras: %w", a.ID, err)
	}
	if err := json.Unmarshal(lock, &a.Lock); err != nil {
		return a, fmt.Errorf("python package %s: lock: %w", a.ID, err)
	}
	return a, nil
}

// Open implements domain.PythonPackageStore.
func (s *PgPythonPackageStore) Open(ctx context.Context, a domain.PythonPackageApproval, actor string) (domain.PythonPackageApproval, bool, error) {
	a.Name = domain.NormalizePythonName(a.Name)
	if a.LockSHA256 == "" {
		a.LockSHA256 = a.Lock.SHA256()
	}
	if a.ID == "" {
		a.ID = domain.PythonPackageApprovalID(a.Target, a.Name, a.Version, a.LockSHA256)
	}
	if a.State == "" {
		a.State = domain.PythonPackagePending
	}
	var out domain.PythonPackageApproval
	created := false
	err := s.recordTx(ctx, a.ID, func(tx pgx.Tx) error {
		cur, err := scanPythonPackage(tx.QueryRow(ctx, `SELECT `+pyPkgCols+` FROM python_package_approvals WHERE id = $1`, a.ID))
		if err == nil {
			out = cur
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("python package: read: %w", err)
		}
		extras, _ := json.Marshal(nonNil(a.Extras)) // strings: cannot fail
		lock, err := json.Marshal(a.Lock)
		if err != nil {
			return fmt.Errorf("python package: lock: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO python_package_approvals (id, target, name, version, extras, lock_sha256, lock,
			base_generation, state, approval_id, requested_by, requested_at, detail)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			a.ID, a.Target, a.Name, a.Version, extras, a.LockSHA256, lock, a.BaseGeneration, a.State, a.ApprovalID,
			a.RequestedBy, a.RequestedAt.UTC(), a.Detail); err != nil {
			return fmt.Errorf("python package: insert: %w", err)
		}
		created, out = true, a
		return appendPythonPackageEvent(ctx, tx, domain.PythonPackageEvent{
			ApprovalID: a.ID, Target: a.Target, Name: a.Name, Version: a.Version,
			Action: domain.PythonPackageActionRequested, Actor: actor, Detail: "lock " + a.LockSHA256, At: a.RequestedAt,
		})
	})
	return out, created, err
}

// Decide implements domain.PythonPackageStore.
func (s *PgPythonPackageStore) Decide(ctx context.Context, id, state, by, detail string, at time.Time) error {
	action := map[string]string{
		domain.PythonPackageApproved: domain.PythonPackageActionApproved,
		domain.PythonPackageDenied:   domain.PythonPackageActionDenied,
		domain.PythonPackageExpired:  domain.PythonPackageActionExpired,
	}[state]
	if action == "" {
		return fmt.Errorf("python package: %q is not a decision", state)
	}
	return s.recordTx(ctx, id, func(tx pgx.Tx) error {
		var target, name, version, cur string
		if err := tx.QueryRow(ctx, `SELECT target, name, version, state FROM python_package_approvals WHERE id = $1`, id).
			Scan(&target, &name, &version, &cur); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrPythonPackageNotFound
			}
			return fmt.Errorf("python package: read: %w", err)
		}
		if cur == state {
			return nil // already decided so: no second audit row
		}
		if _, err := tx.Exec(ctx, `UPDATE python_package_approvals SET state = $2, decided_by = $3, decided_at = $4, detail = $5 WHERE id = $1`,
			id, state, by, at.UTC(), detail); err != nil {
			return fmt.Errorf("python package: decide: %w", err)
		}
		return appendPythonPackageEvent(ctx, tx, domain.PythonPackageEvent{
			ApprovalID: id, Target: target, Name: name, Version: version, Action: action, Actor: by, Detail: detail, At: at,
		})
	})
}

// AttachCard implements domain.PythonPackageStore.
func (s *PgPythonPackageStore) AttachCard(ctx context.Context, id, approvalID string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE python_package_approvals SET approval_id = $2 WHERE id = $1`, id, approvalID)
	if err != nil {
		return fmt.Errorf("python package: attach card: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrPythonPackageNotFound
	}
	return nil
}

// Reopen implements domain.PythonPackageStore.
func (s *PgPythonPackageStore) Reopen(ctx context.Context, id, approvalID, actor string, at time.Time) error {
	return s.recordTx(ctx, id, func(tx pgx.Tx) error {
		var target, name, version string
		if err := tx.QueryRow(ctx, `SELECT target, name, version FROM python_package_approvals WHERE id = $1`, id).
			Scan(&target, &name, &version); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrPythonPackageNotFound
			}
			return fmt.Errorf("python package: read: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE python_package_approvals SET state = 'pending', approval_id = $2, decided_by = '',
			decided_at = NULL, detail = '' WHERE id = $1`, id, approvalID); err != nil {
			return fmt.Errorf("python package: reopen: %w", err)
		}
		return appendPythonPackageEvent(ctx, tx, domain.PythonPackageEvent{
			ApprovalID: id, Target: target, Name: name, Version: version,
			Action: domain.PythonPackageActionRequested, Actor: actor, Detail: "asked again on " + approvalID, At: at,
		})
	})
}

// Get implements domain.PythonPackageStore.
func (s *PgPythonPackageStore) Get(ctx context.Context, id string) (domain.PythonPackageApproval, error) {
	a, err := scanPythonPackage(s.pool.QueryRow(ctx, `SELECT `+pyPkgCols+` FROM python_package_approvals WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return a, domain.ErrPythonPackageNotFound
	}
	return a, err
}

func (s *PgPythonPackageStore) list(ctx context.Context, where string, args ...any) ([]domain.PythonPackageApproval, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+pyPkgCols+` FROM python_package_approvals WHERE `+where+` ORDER BY requested_at DESC, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list python package approvals: %w", err)
	}
	defer rows.Close()
	var out []domain.PythonPackageApproval
	for rows.Next() {
		a, err := scanPythonPackage(rows)
		if err != nil {
			return nil, fmt.Errorf("scan python package approval: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ForTarget implements domain.PythonPackageStore.
func (s *PgPythonPackageStore) ForTarget(ctx context.Context, target string) ([]domain.PythonPackageApproval, error) {
	return s.list(ctx, `target = $1`, target)
}

// ForPackage implements domain.PythonPackageStore.
func (s *PgPythonPackageStore) ForPackage(ctx context.Context, name, version string) ([]domain.PythonPackageApproval, error) {
	return s.list(ctx, `name = $1 AND version = $2`, domain.NormalizePythonName(name), version)
}

// List implements domain.PythonPackageStore.
func (s *PgPythonPackageStore) List(ctx context.Context) ([]domain.PythonPackageApproval, error) {
	return s.list(ctx, `TRUE`)
}

// Remove implements domain.PythonPackageStore: approved → removed, audited
// in the same transaction (migration 0067 admits the state).
func (s *PgPythonPackageStore) Remove(ctx context.Context, id, by, detail, generation string, at time.Time) error {
	return s.recordTx(ctx, id, func(tx pgx.Tx) error {
		var target, name, version, cur string
		if err := tx.QueryRow(ctx, `SELECT target, name, version, state FROM python_package_approvals WHERE id = $1`, id).
			Scan(&target, &name, &version, &cur); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrPythonPackageNotFound
			}
			return fmt.Errorf("python package: read: %w", err)
		}
		if cur != domain.PythonPackageApproved {
			return fmt.Errorf("python package %s is %s, not approved: nothing to remove", id, cur)
		}
		if _, err := tx.Exec(ctx, `UPDATE python_package_approvals SET state = 'removed', decided_by = $2, decided_at = $3, detail = $4 WHERE id = $1`,
			id, by, at.UTC(), detail); err != nil {
			return fmt.Errorf("python package: remove: %w", err)
		}
		return appendPythonPackageEvent(ctx, tx, domain.PythonPackageEvent{
			ApprovalID: id, Target: target, Name: name, Version: version, Action: domain.PythonPackageActionRemoved,
			Actor: by, Detail: detail, Generation: generation, At: at,
		})
	})
}

// AppendEvent implements domain.PythonPackageStore.
func (s *PgPythonPackageStore) AppendEvent(ctx context.Context, e domain.PythonPackageEvent) error {
	return s.recordTx(ctx, e.ApprovalID, func(tx pgx.Tx) error { return appendPythonPackageEvent(ctx, tx, e) })
}

// Events implements domain.PythonPackageStore.
func (s *PgPythonPackageStore) Events(ctx context.Context, recordID string) ([]domain.PythonPackageEvent, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, record_id, seq, target, name, version, action, actor, detail, generation, at
		FROM python_package_events WHERE record_id = $1 ORDER BY seq`, recordID)
	if err != nil {
		return nil, fmt.Errorf("list python package events: %w", err)
	}
	defer rows.Close()
	var out []domain.PythonPackageEvent
	for rows.Next() {
		var e domain.PythonPackageEvent
		if err := rows.Scan(&e.ID, &e.ApprovalID, &e.Seq, &e.Target, &e.Name, &e.Version, &e.Action, &e.Actor, &e.Detail, &e.Generation, &e.At); err != nil {
			return nil, fmt.Errorf("scan python package event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
