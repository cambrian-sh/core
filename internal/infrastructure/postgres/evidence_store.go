package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// PgEvidenceStore is the Postgres adapter for domain.EvidenceStore (ADR-0105).
// Tables come from migration 0011_evidence.sql; this type contains no DDL.
type PgEvidenceStore struct {
	pool *pgxpool.Pool
}

// NewPgEvidenceStore wraps an existing pool. It performs no schema work: the
// migration runner (ADR-0064) owns DDL, and an adapter that creates its own
// tables would hide a missed migration instead of failing on it.
func NewPgEvidenceStore(pool *pgxpool.Pool) *PgEvidenceStore {
	return &PgEvidenceStore{pool: pool}
}

var _ domain.EvidenceStore = (*PgEvidenceStore)(nil)

// Insert implements the idempotent evidence+outbox write (ADR-0105 D2/D3).
//
// One transaction, two statements. ON CONFLICT DO NOTHING carries the
// idempotency: a replayed delivery takes the conflict arm, reads the existing
// row's id, and — critically — inserts NO outbox item, so a replay does not
// enqueue duplicate work either.
func (s *PgEvidenceStore) Insert(ctx context.Context, ev domain.Evidence) (domain.EvidenceID, bool, error) {
	if ev.SourceID == "" || ev.SourceKey == "" {
		return "", false, fmt.Errorf("evidence insert: source_id and source_key are required")
	}
	if ev.ContentHash == "" {
		// The ordering contract (bytes first) makes an empty hash a caller bug,
		// not a data shape: there is nothing this row could ever be reprocessed
		// from.
		return "", false, fmt.Errorf("evidence insert: content_hash is required")
	}
	ns := ev.NamespaceID
	if ns == "" {
		ns = "default"
	}
	id := ev.ID
	if id == "" {
		id = domain.EvidenceID("ev_" + uuid.NewString())
	}
	ingested := ev.IngestedAt
	if ingested.IsZero() {
		ingested = time.Now().UTC()
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful commit

	var srcTime *time.Time
	if !ev.SourceTime.IsZero() {
		srcTime = &ev.SourceTime
	}
	var revises *string
	if ev.RevisesID != "" {
		r := string(ev.RevisesID)
		revises = &r
	}
	tags := ev.Classification
	if tags == nil {
		tags = []string{}
	}

	var gotID string
	err = tx.QueryRow(ctx, `
		INSERT INTO evidence (id, namespace_id, source_id, source_key, source_revision,
			source_time, ingested_at, content_hash, content_bytes, classification,
			cursor, trace_id, revises_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT ON CONSTRAINT evidence_source_revision_unique DO NOTHING
		RETURNING id`,
		string(id), ns, ev.SourceID, ev.SourceKey, ev.SourceRevision,
		srcTime, ingested, string(ev.ContentHash), ev.ContentBytes, tags,
		ev.Cursor, ev.TraceID, revises,
	).Scan(&gotID)

	switch {
	case err == nil:
		// Fresh row: enqueue its work item in the same transaction, so evidence
		// and outbox are atomically both-or-neither (memo §11).
		if _, err := tx.Exec(ctx,
			`INSERT INTO evidence_outbox (namespace_id, evidence_id) VALUES ($1,$2)`,
			ns, gotID); err != nil {
			return "", false, err
		}
		if err := tx.Commit(ctx); err != nil {
			return "", false, err
		}
		return domain.EvidenceID(gotID), true, nil

	case errors.Is(err, pgx.ErrNoRows):
		// Conflict arm: the delivery was replayed. Return the existing identity
		// so the caller can correlate, and write nothing anywhere.
		if err := tx.QueryRow(ctx, `
			SELECT id FROM evidence
			WHERE namespace_id=$1 AND source_id=$2 AND source_key=$3 AND source_revision=$4`,
			ns, ev.SourceID, ev.SourceKey, ev.SourceRevision,
		).Scan(&gotID); err != nil {
			return "", false, fmt.Errorf("evidence replay lookup: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return "", false, err
		}
		return domain.EvidenceID(gotID), false, nil

	default:
		return "", false, err
	}
}

// Get returns one evidence row.
func (s *PgEvidenceStore) Get(ctx context.Context, id domain.EvidenceID) (*domain.Evidence, error) {
	var (
		ev      domain.Evidence
		gotID   string
		srcTime *time.Time
		revises *string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id, namespace_id, source_id, source_key, source_revision,
			source_time, ingested_at, content_hash, content_bytes, classification,
			cursor, trace_id, revises_id
		FROM evidence WHERE id = $1`, string(id),
	).Scan(&gotID, &ev.NamespaceID, &ev.SourceID, &ev.SourceKey, &ev.SourceRevision,
		&srcTime, &ev.IngestedAt, (*string)(&ev.ContentHash), &ev.ContentBytes,
		&ev.Classification, &ev.Cursor, &ev.TraceID, &revises)
	if err != nil {
		return nil, fmt.Errorf("evidence get %s: %w", id, err)
	}
	ev.ID = domain.EvidenceID(gotID)
	if srcTime != nil {
		ev.SourceTime = *srcTime
	}
	if revises != nil {
		ev.RevisesID = domain.EvidenceID(*revises)
	}
	return &ev, nil
}

// PendingOutbox lists unconsumed items, oldest first. Listing never claims.
func (s *PgEvidenceStore) PendingOutbox(ctx context.Context, limit int) ([]domain.EvidenceOutboxItem, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, namespace_id, evidence_id, created_at
		FROM evidence_outbox
		WHERE processed_at IS NULL
		ORDER BY created_at, id
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.EvidenceOutboxItem
	for rows.Next() {
		var it domain.EvidenceOutboxItem
		var evID string
		if err := rows.Scan(&it.ID, &it.NamespaceID, &evID, &it.CreatedAt); err != nil {
			return nil, err
		}
		it.EvidenceID = domain.EvidenceID(evID)
		out = append(out, it)
	}
	return out, rows.Err()
}

// MarkProcessed is the exactly-once-logical transition (ADR-0105 D3): the
// conditional UPDATE's row count is the truth, so of N racing consumers
// exactly one observes true.
func (s *PgEvidenceStore) MarkProcessed(ctx context.Context, outboxID int64) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE evidence_outbox SET processed_at = now() WHERE id = $1 AND processed_at IS NULL`,
		outboxID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
