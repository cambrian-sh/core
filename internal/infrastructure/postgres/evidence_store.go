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

var (
	_ domain.EvidenceStore        = (*PgEvidenceStore)(nil)
	_ domain.EvidenceErasureFence = (*PgEvidenceStore)(nil)
)

// SourceErased implements domain.EvidenceErasureFence: whether a tombstone of
// sourceKey was written after the ingest origin ctx carries (ADR-0137 D5). Advisory
// only — Insert re-checks under the per-document lock.
func (s *PgEvidenceStore) SourceErased(ctx context.Context, sourceKey string) (bool, error) {
	return erasedAfterOrigin(ctx, s.pool, sourceKey, "")
}

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
	ns = orDefaultNamespace(ns)
	id := ev.ID
	if id == "" {
		id = domain.EvidenceID("ev_" + uuid.NewString())
	}
	// ingested_at is the DATABASE's clock (now(), via COALESCE below) unless the
	// caller supplies one, so every "ingested before X" comparison against another
	// database-stamped time (a backfill's started_at, ADR-0147 D14) reads one clock.
	// The host clock used to stamp it, and host/database skew could skip a real
	// deletion or tombstone a key that arrived live during a fill
	// (T-20261113-a-reconciling-fill-is-reported-caught-up-before; owner option (a)).
	var ingested *time.Time
	if !ev.IngestedAt.IsZero() {
		at := ev.IngestedAt
		ingested = &at
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful commit

	// ADR-0137 D5 no-resurrection fence (ENG-1866). An erase deletes the evidence
	// whose source_key is an erased document id and leaves a tombstone keyed by that
	// id (memory_erasure_targets.document_id), so the fence keys on source_key: it
	// takes the per-document lock in shared mode (the erase holds it exclusively)
	// and refuses with domain.ErrSourceErased when the tombstone is younger than the
	// ingest origin on ctx — a reactive journal replay, a retried delivery or a
	// queued ingest that predates the erase. A delivery accepted after the erase is
	// a new act and passes (ADR-0137 Q4); a write with no origin only takes the lock.
	if err := erasureFenceTx(ctx, tx, ev.SourceKey, ""); err != nil {
		return "", false, fmt.Errorf("evidence insert: %w", err)
	}

	var srcTime *time.Time
	if !ev.SourceTime.IsZero() {
		srcTime = &ev.SourceTime
	}
	var revises *string
	if ev.RevisesID != "" {
		r := string(ev.RevisesID)
		revises = &r
	}
	var derived *string
	if ev.DerivedFrom != "" {
		d := string(ev.DerivedFrom)
		derived = &d
	}
	tags := ev.Classification
	if tags == nil {
		tags = []string{}
	}
	// Empty rather than NULL, matching classification: the column is NOT NULL and
	// "nobody is a party to this" is the honest reading of a record whose source
	// named none (ADR-0121 D2).
	parties := ev.Parties
	if parties == nil {
		parties = []string{}
	}

	var gotID string
	err = tx.QueryRow(ctx, `
		INSERT INTO evidence (id, namespace_id, source_id, source_key, source_revision,
			source_time, ingested_at, content_hash, content_bytes, classification,
			parties, cursor, trace_id, revises_id, derived_from)
		VALUES ($1,$2,$3,$4,$5,$6,COALESCE($7::timestamptz, now()),$8,$9,$10,$11,$12,$13,$14,$15)
		ON CONFLICT ON CONSTRAINT evidence_source_revision_unique DO NOTHING
		RETURNING id`,
		string(id), ns, ev.SourceID, ev.SourceKey, ev.SourceRevision,
		srcTime, ingested, string(ev.ContentHash), ev.ContentBytes, tags,
		parties, ev.Cursor, ev.TraceID, revises, derived,
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
		derived *string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id, namespace_id, source_id, source_key, source_revision,
			source_time, ingested_at, content_hash, content_bytes, classification,
			parties, cursor, trace_id, revises_id, derived_from
		FROM evidence WHERE id = $1`, string(id),
	).Scan(&gotID, &ev.NamespaceID, &ev.SourceID, &ev.SourceKey, &ev.SourceRevision,
		&srcTime, &ev.IngestedAt, (*string)(&ev.ContentHash), &ev.ContentBytes,
		&ev.Classification, &ev.Parties, &ev.Cursor, &ev.TraceID, &revises, &derived)
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
	if derived != nil {
		ev.DerivedFrom = domain.EvidenceID(*derived)
	}
	return &ev, nil
}

// LatestBySourceKey returns the newest evidence row for one source artifact, or
// nil when the source never sent that key (ADR-0147 D7). Newest is INGEST
// order: source_revision is a content digest and orders nothing.
// idx_evidence_source (namespace_id, source_id, source_key) serves the lookup.
func (s *PgEvidenceStore) LatestBySourceKey(ctx context.Context, namespace, sourceID, sourceKey string) (*domain.Evidence, error) {
	namespace = orDefaultNamespace(namespace)
	var id string
	err := s.pool.QueryRow(ctx, `
		SELECT id FROM evidence
		WHERE namespace_id = $1 AND source_id = $2 AND source_key = $3
		ORDER BY ingested_at DESC, id DESC LIMIT 1`,
		namespace, sourceID, sourceKey).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("evidence latest by source key: %w", err)
	}
	return s.Get(ctx, domain.EvidenceID(id))
}

// PendingOutbox lists the DUE items, oldest first (migration 0039). Listing
// never claims.
//
// Two branches, each off its own partial index, merged by created_at: rows that
// are not held (fresh, or retrying once their backoff has passed — filtered in
// created_at order, so a short backoff tail is all that is skipped) and held
// rows whose safety recheck has come due. A held backlog therefore costs the
// read nothing until it is released or rechecked: it is not in the first index
// at all (T-20261112-evidence-outbox-is-head-of-line-blocked-the).
func (s *PgEvidenceStore) PendingOutbox(ctx context.Context, limit int) ([]domain.EvidenceOutboxItem, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, namespace_id, evidence_id, created_at, attempts FROM (
			(SELECT id, namespace_id, evidence_id, created_at, attempts FROM evidence_outbox
			 WHERE processed_at IS NULL AND dead_at IS NULL AND held_key = ''
			   AND (next_attempt_at IS NULL OR next_attempt_at <= now())
			 ORDER BY created_at, id LIMIT $1)
			UNION ALL
			(SELECT id, namespace_id, evidence_id, created_at, attempts FROM evidence_outbox
			 WHERE processed_at IS NULL AND dead_at IS NULL AND held_key <> ''
			   AND next_attempt_at <= now()
			 ORDER BY next_attempt_at LIMIT $1)
		) due
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
		if err := rows.Scan(&it.ID, &it.NamespaceID, &evID, &it.CreatedAt, &it.Attempts); err != nil {
			return nil, err
		}
		it.EvidenceID = domain.EvidenceID(evID)
		out = append(out, it)
	}
	return out, rows.Err()
}

// RecordOutboxFailure writes one failed attempt. The next-attempt time is
// computed by the DATABASE clock (now() + retryAfter), the same clock the
// pending read compares against, so host/DB clock skew cannot make a backoff
// longer or shorter. A failure clears any hold: the item failed, it is not
// waiting on a release.
func (s *PgEvidenceStore) RecordOutboxFailure(ctx context.Context, outboxID int64, attempts int, lastErr string, retryAfter time.Duration, dead bool) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE evidence_outbox SET
			attempts = $2, last_error = $3, held_key = '',
			next_attempt_at = CASE WHEN $5 THEN NULL ELSE now() + make_interval(secs => $4) END,
			dead_at = CASE WHEN $5 THEN now() ELSE NULL END
		WHERE id = $1 AND processed_at IS NULL`,
		outboxID, attempts, lastErr, retryAfter.Seconds(), dead)
	if err != nil {
		return fmt.Errorf("evidence outbox: record failure: %w", err)
	}
	return nil
}

// HoldOutbox parks a not-ready item: out of the pending read until released
// or rechecked, attempts untouched.
func (s *PgEvidenceStore) HoldOutbox(ctx context.Context, outboxID int64, holdKey, reason string, recheckAfter time.Duration) error {
	if holdKey == "" {
		return fmt.Errorf("evidence outbox: hold: a hold key is required")
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE evidence_outbox SET
			held_key = $2, last_error = $3,
			next_attempt_at = now() + make_interval(secs => $4)
		WHERE id = $1 AND processed_at IS NULL AND dead_at IS NULL`,
		outboxID, holdKey, reason, recheckAfter.Seconds())
	if err != nil {
		return fmt.Errorf("evidence outbox: hold: %w", err)
	}
	return nil
}

// ReleaseHeldOutbox makes every unprocessed item held under holdKey due now.
func (s *PgEvidenceStore) ReleaseHeldOutbox(ctx context.Context, holdKey string) (int, error) {
	if holdKey == "" {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE evidence_outbox SET held_key = '', next_attempt_at = NULL
		WHERE held_key = $1 AND processed_at IS NULL AND dead_at IS NULL`, holdKey)
	if err != nil {
		return 0, fmt.Errorf("evidence outbox: release %q: %w", holdKey, err)
	}
	return int(tag.RowsAffected()), nil
}

// DeadOutbox lists dead items, most recently dead first, optionally for one
// evidence source.
func (s *PgEvidenceStore) DeadOutbox(ctx context.Context, sourceID string, limit int) ([]domain.DeadEvidenceOutboxItem, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT o.id, o.namespace_id, o.evidence_id, o.created_at, o.attempts,
		       e.source_id, o.last_error, o.dead_at
		FROM evidence_outbox o JOIN evidence e ON e.id = o.evidence_id
		WHERE o.processed_at IS NULL AND o.dead_at IS NOT NULL
		  AND ($1 = '' OR e.source_id = $1)
		ORDER BY o.dead_at DESC, o.id DESC
		LIMIT $2`, sourceID, limit)
	if err != nil {
		return nil, fmt.Errorf("evidence outbox: dead: %w", err)
	}
	defer rows.Close()
	var out []domain.DeadEvidenceOutboxItem
	for rows.Next() {
		var it domain.DeadEvidenceOutboxItem
		var evID string
		if err := rows.Scan(&it.ID, &it.NamespaceID, &evID, &it.CreatedAt, &it.Attempts,
			&it.SourceID, &it.LastError, &it.DeadAt); err != nil {
			return nil, err
		}
		it.EvidenceID = domain.EvidenceID(evID)
		out = append(out, it)
	}
	return out, rows.Err()
}

// ReplayDeadOutbox makes a source's dead items pending again (see the port).
// The CTE locks the dead rows and the UPDATE returns their values from BEFORE
// the reset, so the caller can say exactly what it replayed (attempts, last
// error, dead time) — the audit's "how many" and "which". The `dead_at IS NOT
// NULL` guard is the idempotence: a second replay, or a racing one, finds
// nothing left to reset. held_key is cleared too, so the row lands in the
// ready index (idx_evidence_outbox_ready) in its original created_at order.
func (s *PgEvidenceStore) ReplayDeadOutbox(ctx context.Context, sourceID string, evidenceID domain.EvidenceID) ([]domain.DeadEvidenceOutboxItem, error) {
	if sourceID == "" {
		return nil, fmt.Errorf("evidence outbox: replay dead: a source id is required")
	}
	rows, err := s.pool.Query(ctx, `
		WITH d AS (
			SELECT o.id, o.attempts, o.last_error, o.dead_at, e.source_id
			FROM evidence_outbox o JOIN evidence e ON e.id = o.evidence_id
			WHERE o.processed_at IS NULL AND o.dead_at IS NOT NULL
			  AND e.source_id = $1 AND ($2 = '' OR o.evidence_id = $2)
			FOR UPDATE OF o
		)
		UPDATE evidence_outbox o SET
			attempts = 0, last_error = '', held_key = '', next_attempt_at = NULL, dead_at = NULL
		FROM d
		WHERE o.id = d.id
		RETURNING o.id, o.namespace_id, o.evidence_id, o.created_at, d.attempts, d.source_id, d.last_error, d.dead_at`,
		sourceID, string(evidenceID))
	if err != nil {
		return nil, fmt.Errorf("evidence outbox: replay dead %q: %w", sourceID, err)
	}
	defer rows.Close()
	var out []domain.DeadEvidenceOutboxItem
	for rows.Next() {
		var it domain.DeadEvidenceOutboxItem
		var evID string
		if err := rows.Scan(&it.ID, &it.NamespaceID, &evID, &it.CreatedAt, &it.Attempts,
			&it.SourceID, &it.LastError, &it.DeadAt); err != nil {
			return nil, fmt.Errorf("evidence outbox: replay dead %q: %w", sourceID, err)
		}
		it.EvidenceID = domain.EvidenceID(evID)
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("evidence outbox: replay dead %q: %w", sourceID, err)
	}
	return out, nil
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
