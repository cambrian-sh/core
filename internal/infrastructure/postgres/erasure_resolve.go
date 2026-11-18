package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0137 S3–S5 (ENG-411 stage B): candidate resolution for the authorization
// decision, the outbox the erasure worker drains, and the no-resurrection fence every
// content write path takes.

// Resolve implements domain.MemoryEraser: the stored records sel names, content-free.
// It reads outside the erase transaction; the enforcing eraser decides on it and then
// calls Erase with the same concrete ids, which re-resolves them under the per-document
// locks.
func (s *PgErasureStore) Resolve(ctx context.Context, sel domain.ErasureSelector) ([]domain.ErasureCandidate, error) {
	sel = sel.Normalized()
	q := s.l.k.pool
	var out []domain.ErasureCandidate

	docs := func(via string, ids []string) error {
		if len(ids) == 0 {
			return nil
		}
		rows, err := q.Query(ctx, `SELECT id, COALESCE(written_by, ''), COALESCE(tags, '{}') FROM `+TableDocuments+`
			WHERE id = ANY($1) ORDER BY id`, ids)
		if err != nil {
			return fmt.Errorf("erasure resolve: documents: %w", err)
		}
		found := map[string]bool{}
		for rows.Next() {
			var c domain.ErasureCandidate
			if err := rows.Scan(&c.ID, &c.WrittenBy, &c.Tags); err != nil {
				rows.Close()
				return err
			}
			c.Kind, c.DocumentID, c.Via = domain.ErasureTargetDocument, c.ID, orID(via, c.ID)
			found[c.ID] = true
			out = append(out, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		// A document that survives only as evidence or as an ADR-0093 frozen copy has no
		// writer of record and no tags: an operator-only candidate (ADR-0137 D3).
		var missing []string
		for _, id := range ids {
			if !found[id] {
				missing = append(missing, id)
			}
		}
		if len(missing) == 0 {
			return nil
		}
		hasLegacy, err := tableExists(ctx, q, "documents_legacy")
		if err != nil {
			return err
		}
		sql := `SELECT DISTINCT d FROM unnest($1::text[]) d WHERE EXISTS (SELECT 1 FROM evidence e WHERE e.source_key = d)`
		if hasLegacy {
			sql += ` OR EXISTS (SELECT 1 FROM documents_legacy l WHERE l.id = d OR l.metadata->>'document_id' = d)`
		}
		rest, err := collectIDs(ctx, q, sql+` ORDER BY 1`, missing)
		if err != nil {
			return fmt.Errorf("erasure resolve: residual documents: %w", err)
		}
		for _, id := range rest {
			out = append(out, domain.ErasureCandidate{Kind: domain.ErasureTargetDocument, ID: id, DocumentID: id, Via: orID(via, id)})
		}
		return nil
	}

	if err := docs("", sel.DocumentIDs); err != nil {
		return nil, err
	}
	for _, th := range sel.ThreadIDs {
		ids, err := collectIDs(ctx, q, `SELECT document_id FROM document_associations WHERE session_id = $1
			UNION SELECT document_id FROM `+TableChunks+`
			 WHERE metadata->>'`+domain.MetaIngestThreadID+`' = $1 AND document_id IS NOT NULL
			ORDER BY 1`, th)
		if err != nil {
			return nil, fmt.Errorf("erasure resolve: thread: %w", err)
		}
		if err := docs(th, ids); err != nil {
			return nil, err
		}
	}
	if len(sel.ChunkIDs) > 0 {
		// A chunk of a stored document resolves to that DOCUMENT (Via = the chunk id):
		// the erase widens to the whole document (D2 as amended 2026-11-17, ENG-1947), so
		// the preview names what will go, and the writer of record and the authoritative
		// tags are the document's (ADR-0093). A document-less chunk — or one whose
		// document row is gone — is its own candidate with the writer the write tier
		// stamped on it and its own tag copy.
		rows, err := q.Query(ctx, `SELECT c.id, COALESCE(c.document_id, ''), d.id IS NOT NULL,
				COALESCE(NULLIF(d.written_by, ''), c.metadata->>'`+domain.MetaWrittenBy+`', ''),
				COALESCE(c.metadata->'tags', '[]'::jsonb), COALESCE(d.tags, '{}')
			FROM `+TableChunks+` c LEFT JOIN `+TableDocuments+` d ON d.id = c.document_id
			WHERE c.id = ANY($1) ORDER BY c.id`, sel.ChunkIDs)
		if err != nil {
			return nil, fmt.Errorf("erasure resolve: chunks: %w", err)
		}
		for rows.Next() {
			var (
				c       domain.ErasureCandidate
				hasDoc  bool
				tags    []byte
				docTags []string
			)
			if err := rows.Scan(&c.ID, &c.DocumentID, &hasDoc, &c.WrittenBy, &tags, &docTags); err != nil {
				rows.Close()
				return nil, err
			}
			if hasDoc {
				c.Kind, c.Via, c.ID, c.Tags = domain.ErasureTargetDocument, c.ID, c.DocumentID, docTags
			} else {
				c.Kind, c.Via = domain.ErasureTargetChunk, c.ID
				c.Tags = jsonTags(tags)
			}
			out = append(out, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	if len(sel.ExperienceIDs) > 0 {
		rows, err := q.Query(ctx, `SELECT id, COALESCE(tags, '{}') FROM `+TableExperiences+` WHERE id = ANY($1) ORDER BY id`, sel.ExperienceIDs)
		if err != nil {
			return nil, fmt.Errorf("erasure resolve: experiences: %w", err)
		}
		for rows.Next() {
			var c domain.ErasureCandidate
			if err := rows.Scan(&c.ID, &c.Tags); err != nil {
				rows.Close()
				return nil, err
			}
			c.Kind, c.Via = domain.ErasureTargetExperience, c.ID
			out = append(out, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	for _, ref := range sel.EntityRefs {
		var ok bool
		if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM knowledge_items ki WHERE ki.entity_id = $1
			OR EXISTS (SELECT 1 FROM statement_values sv WHERE sv.item_id = ki.id
				AND sv.value_type = 'entity' AND sv.value_entity_id = $1))`, ref).Scan(&ok); err != nil {
			return nil, fmt.Errorf("erasure resolve: entity: %w", err)
		}
		if ok {
			out = append(out, domain.ErasureCandidate{Kind: domain.ErasureTargetEntity, ID: ref, Via: ref})
		}
	}
	return out, nil
}

func orID(via, id string) string {
	if via != "" {
		return via
	}
	return id
}

// jsonTags decodes a JSONB tag array; anything else is no tags.
func jsonTags(raw []byte) []string {
	var tags []string
	if len(raw) == 0 || json.Unmarshal(raw, &tags) != nil {
		return nil
	}
	return tags
}

// PendingSteps implements domain.ErasureOutbox.
func (s *PgErasureStore) PendingSteps(ctx context.Context, limit, maxAttempts int) ([]domain.ErasureOutboxRow, error) {
	rows, err := s.l.k.pool.Query(ctx, `SELECT id, request_id, step, ref, attempts FROM memory_erasure_outbox
		WHERE processed_at IS NULL AND attempts < $2 ORDER BY created_at, id LIMIT $1`, limit, maxAttempts)
	if err != nil {
		return nil, mapError("PendingSteps", err)
	}
	defer rows.Close()
	var out []domain.ErasureOutboxRow
	for rows.Next() {
		var r domain.ErasureOutboxRow
		if err := rows.Scan(&r.ID, &r.RequestID, &r.Step, &r.Ref, &r.Attempts); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CIDStillReferenced implements domain.ErasureOutbox: evidence with the same content hash,
// or a surviving source-document stub naming the CID, keeps the blob.
func (s *PgErasureStore) CIDStillReferenced(ctx context.Context, cid string) (bool, error) {
	var ok bool
	err := s.l.k.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM evidence WHERE content_hash = $1)
		OR EXISTS (SELECT 1 FROM `+TableChunks+` WHERE metadata->>'content_cid' = $1)`, cid).Scan(&ok)
	if err != nil {
		return false, mapError("CIDStillReferenced", err)
	}
	return ok, nil
}

// ErasedRecords implements domain.ErasureOutbox: the request's tombstones (ids by kind,
// document ids) and the CIDs of its cas_delete steps, processed or not.
func (s *PgErasureStore) ErasedRecords(ctx context.Context, requestID string) (domain.ErasedRecords, error) {
	out := domain.ErasedRecords{RequestID: requestID, Targets: map[string][]string{}}
	rows, err := s.l.k.pool.Query(ctx, `SELECT target_kind, target_id, COALESCE(document_id, '')
		FROM memory_erasure_targets WHERE request_id = $1 ORDER BY target_kind, target_id`, requestID)
	if err != nil {
		return out, mapError("ErasedRecords", err)
	}
	docs := map[string]bool{}
	for rows.Next() {
		var kind, id, doc string
		if err := rows.Scan(&kind, &id, &doc); err != nil {
			rows.Close()
			return out, mapError("ErasedRecords", err)
		}
		out.Targets[kind] = append(out.Targets[kind], id)
		if doc != "" {
			docs[doc] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, mapError("ErasedRecords", err)
	}
	for d := range docs {
		out.DocumentIDs = append(out.DocumentIDs, d)
	}
	sort.Strings(out.DocumentIDs)
	crows, err := s.l.k.pool.Query(ctx, `SELECT ref FROM memory_erasure_outbox
		WHERE request_id = $1 AND step = $2 ORDER BY ref`, requestID, domain.ErasureStepCASDelete)
	if err != nil {
		return out, mapError("ErasedRecords", err)
	}
	defer crows.Close()
	for crows.Next() {
		var c string
		if err := crows.Scan(&c); err != nil {
			return out, mapError("ErasedRecords", err)
		}
		out.CIDs = append(out.CIDs, c)
	}
	return out, mapError("ErasedRecords", crows.Err())
}

// VaultBodyStillReferenced implements domain.ErasureOutbox: a surviving run-ledger
// entry still names the vault body (the table may be absent on a kernel without
// the ledger: then nothing names it).
func (s *PgErasureStore) VaultBodyStillReferenced(ctx context.Context, hash string) (bool, error) {
	has, err := tableExists(ctx, s.l.k.pool, "ledger_entries")
	if err != nil || !has {
		return false, err
	}
	var ok bool
	err = s.l.k.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ledger_entries WHERE body_hash = $1)`, hash).Scan(&ok)
	if err != nil {
		return false, mapError("VaultBodyStillReferenced", err)
	}
	return ok, nil
}

// MarkStepDone implements domain.ErasureOutbox (exactly once).
func (s *PgErasureStore) MarkStepDone(ctx context.Context, id int64) (bool, error) {
	tag, err := s.l.k.pool.Exec(ctx, `UPDATE memory_erasure_outbox SET processed_at = now()
		WHERE id = $1 AND processed_at IS NULL`, id)
	if err != nil {
		return false, mapError("MarkStepDone", err)
	}
	return tag.RowsAffected() == 1, nil
}

// MarkStepFailed implements domain.ErasureOutbox.
func (s *PgErasureStore) MarkStepFailed(ctx context.Context, id int64, errClass string) error {
	if _, err := s.l.k.pool.Exec(ctx, `UPDATE memory_erasure_outbox SET attempts = attempts + 1, last_error = $2
		WHERE id = $1 AND processed_at IS NULL`, id, errClass); err != nil {
		return mapError("MarkStepFailed", err)
	}
	return nil
}

// MarkStepNotErasable implements domain.ErasureOutbox (exactly once): the row is
// processed — nothing more can be done — and keeps the outcome class for the receipt.
func (s *PgErasureStore) MarkStepNotErasable(ctx context.Context, id int64, outcome string) (bool, error) {
	tag, err := s.l.k.pool.Exec(ctx, `UPDATE memory_erasure_outbox
		SET processed_at = now(), attempts = attempts + 1, last_error = $2
		WHERE id = $1 AND processed_at IS NULL`, id, outcome)
	if err != nil {
		return false, mapError("MarkStepNotErasable", err)
	}
	return tag.RowsAffected() == 1, nil
}

// CompleteRequests implements domain.ErasureOutbox.
func (s *PgErasureStore) CompleteRequests(ctx context.Context) (int, error) {
	tag, err := s.l.k.pool.Exec(ctx, `UPDATE memory_erasures e SET status = $1, completed_at = now()
		WHERE e.status = $2 AND NOT EXISTS (SELECT 1 FROM memory_erasure_outbox o
			WHERE o.request_id = e.request_id AND o.processed_at IS NULL)`,
		domain.ErasureStatusComplete, domain.ErasureStatusPendingAsync)
	if err != nil {
		return 0, mapError("CompleteRequests", err)
	}
	return int(tag.RowsAffected()), nil
}

// PruneTombstones implements domain.ErasureOutbox. The receipt row itself is kept: it is
// the record that an erasure happened; only the per-unit fence rows go.
func (s *PgErasureStore) PruneTombstones(ctx context.Context, cutoff time.Time) (int, error) {
	tag, err := s.l.k.pool.Exec(ctx, `DELETE FROM memory_erasure_targets WHERE erased_at < $1`, cutoff)
	if err != nil {
		return 0, mapError("PruneTombstones", err)
	}
	return int(tag.RowsAffected()), nil
}

// erasureFenceTx is the ADR-0137 D5 no-resurrection fence, run inside the writer's
// transaction before it writes content of documentID:
//
//  1. it takes the per-document advisory lock in SHARED mode — the erase transaction
//     takes the same key exclusively (PgErasureStore.eraseTx), so a write and an erase
//     of one document serialise: a write that started first commits before the erase
//     deletes it; a write that waits sees the committed tombstone;
//  2. when ctx carries an ingest origin (domain.IngestOriginFrom), it refuses with
//     domain.ErrSourceErased if a tombstone of the document (or of digest, a document's
//     content digest) was written after that origin — a replay, retry or queued job that
//     predates the erase. A genuinely new ingest (origin after the erase) passes: a
//     re-ingest is a new act (ADR-0137 Q4).
//
// Kernel-authored writes carry no origin and only take the lock. An empty documentID is
// a no-op.
func erasureFenceTx(ctx context.Context, q fenceQuerier, documentID, digest string) error {
	if documentID == "" {
		return nil
	}
	if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(hashtext($1))`, documentID); err != nil {
		return fmt.Errorf("erasure fence: lock: %w", err)
	}
	erased, err := erasedAfterOrigin(ctx, q, documentID, digest)
	if err != nil {
		return err
	}
	if erased {
		return fmt.Errorf("document %s: %w", documentID, domain.ErrSourceErased)
	}
	return nil
}

// erasedAfterOrigin reports whether a tombstone of documentID (or of digest) was
// written after the ingest origin ctx carries; false when ctx carries none. It takes
// no lock: erasureFenceTx calls it under the shared per-document lock, and
// PgEvidenceStore.SourceErased uses it as an early, advisory answer.
func erasedAfterOrigin(ctx context.Context, q rowQuerier, documentID, digest string) (bool, error) {
	origin, ok := domain.IngestOriginFrom(ctx)
	if !ok || documentID == "" {
		return false, nil
	}
	var erased bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM memory_erasure_targets
		WHERE (document_id = $1 OR (NULLIF($2, '') IS NOT NULL AND content_digest = $2)) AND erased_at > $3)`,
		documentID, digest, origin).Scan(&erased); err != nil {
		return false, fmt.Errorf("erasure fence: tombstone: %w", err)
	}
	return erased, nil
}

// rowQuerier is the read half of fenceQuerier.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// fenceQuerier is what erasureFenceTx needs of a transaction.
type fenceQuerier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// isForeignKeyViolation reports a Postgres FK violation (23503) on constraint.
func isForeignKeyViolation(err error, constraint string) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23503" && (constraint == "" || pg.ConstraintName == constraint)
}
