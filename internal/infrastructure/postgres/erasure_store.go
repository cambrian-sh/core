package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/cambrian-sh/core/domain"
)

// PgErasureStore is the raw ADR-0137 memory eraser (S2): ONE Postgres transaction per
// request deletes the named records and every derivative a principal-serving read can
// touch (D4), and leaves only a content-free receipt, one tombstone row per erased unit
// and the outbox rows of the async tail (D1, D7).
//
// It composes the existing pieces instead of re-implementing them (AGENTS.md §4.3): the
// ADR-0138 document cascade (PgDocumentLifecycle.retireDocumentsTx — fact items,
// markers, extraction journal, KG triplets, PageRank, edges, sections, chunks) and the
// substrate's one erasure (PgKnowledgeStore.EraseItemsTx — items, assessments,
// resolutions, ADR-0137 Q5 sentinels, re-derivation). Around them it deletes the
// documents rows, their version history and association rows, the ADR-0093
// documents_legacy copies, experience parents (ADR-0095 D6), single chunks, and the
// ADR-0105 evidence of the documents with its dependents.
//
// It ENFORCES NOTHING about who may erase what: it is a tier-0 component. The kernel holds
// it unexported (kernel.MemoryStack) until the S3 authz.EnforcingEraser wraps it; only
// tests call it in stage A (ENG-411).
//
// Not in this transaction (yet): premium rows that reference evidence (ingress studio)
// — the ErasureParticipant hook is S7; an erase whose evidence premium still references
// fails on the evidence FK and rolls back, erasing nothing. Generated objects_<kind> rows
// projected from erased events are not rebuilt here (ticketed).
type PgErasureStore struct {
	l *PgDocumentLifecycle
}

// NewPgErasureStore builds the eraser over the document lifecycle (and through it the
// knowledge store and its pool).
func NewPgErasureStore(l *PgDocumentLifecycle) *PgErasureStore {
	if l == nil || l.k == nil {
		panic("postgres.NewPgErasureStore: nil document lifecycle")
	}
	return &PgErasureStore{l: l}
}

var _ domain.MemoryEraser = (*PgErasureStore)(nil)

// Store names the erasure counts use beyond the document cascade's (cascade* constants).
const (
	eraseDocuments        = "documents"
	eraseDocumentVersions = "document_versions"
	eraseDocumentsLegacy  = "documents_legacy"
	eraseExperiences      = "experiences"
	eraseEvidence         = "evidence"
	eraseEvidenceOutbox   = "evidence_outbox"
	eraseEvents           = "events"
	eraseObservations     = "observations"
	eraseLinks            = "links"
)

// erasureTarget is one tombstone row (content-free).
type erasureTarget struct {
	kind, id, documentID, digest string
}

// erasureRun accumulates one transaction's work.
type erasureRun struct {
	tx      pgx.Tx
	counts  map[string]int
	targets []erasureTarget
	cids    map[string]bool
}

func (r *erasureRun) exec(ctx context.Context, store, sql string, args ...any) error {
	tag, err := r.tx.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("erase %s: %w", store, err)
	}
	if n := int(tag.RowsAffected()); n > 0 {
		r.counts[store] += n
	}
	return nil
}

func (r *erasureRun) target(kind, id, documentID, digest string) {
	r.targets = append(r.targets, erasureTarget{kind: kind, id: id, documentID: documentID, digest: digest})
}

// Erase implements domain.MemoryEraser.
func (s *PgErasureStore) Erase(ctx context.Context, req domain.ErasureRequest) (domain.ErasureReceipt, error) {
	if err := req.Validate(); err != nil {
		return domain.ErasureReceipt{}, err
	}
	req.Selector = req.Selector.Normalized()
	id := req.ID()

	tx, err := s.l.k.pool.Begin(ctx)
	if err != nil {
		return domain.ErasureReceipt{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful commit

	// Two concurrent runs of one request serialize here; the second finds the first's
	// receipt (D6).
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, id); err != nil {
		return domain.ErasureReceipt{}, fmt.Errorf("erase: lock request: %w", err)
	}
	if rec, found, err := receiptFrom(ctx, tx, id); err != nil {
		return domain.ErasureReceipt{}, err
	} else if found {
		rec.Replayed = true
		return rec, tx.Commit(ctx)
	}

	run := &erasureRun{tx: tx, counts: map[string]int{}, cids: map[string]bool{}}
	matched, err := s.eraseTx(ctx, run, req.Selector)
	if err != nil {
		return domain.ErasureReceipt{}, err
	}
	if !matched {
		earlier, err := earlierErasure(ctx, tx, req.Selector)
		if err != nil {
			return domain.ErasureReceipt{}, err
		}
		return domain.ErasureReceipt{}, &domain.NothingToEraseError{EarlierRequestID: earlier}
	}

	cids := make([]string, 0, len(run.cids))
	for c := range run.cids {
		cids = append(cids, c)
	}
	sort.Strings(cids)
	status := domain.ErasureStatusComplete
	if len(cids) > 0 {
		status = domain.ErasureStatusPendingAsync
	}
	cj, _ := json.Marshal(run.counts) // map[string]int always marshals
	var erasedAt time.Time
	if err := tx.QueryRow(ctx, `INSERT INTO memory_erasures
			(request_id, principal, principal_kind, reason_code, selector_kinds, status, counts, completed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, CASE WHEN $6 = 'complete' THEN now() END)
		RETURNING erased_at`,
		id, req.Principal, string(req.PrincipalKind), string(req.Reason), req.Selector.Kinds(), status, string(cj)).
		Scan(&erasedAt); err != nil {
		return domain.ErasureReceipt{}, mapError("Erase: receipt", err)
	}
	seen := map[[2]string]bool{}
	for _, t := range run.targets {
		k := [2]string{t.kind, t.id}
		if seen[k] {
			continue
		}
		seen[k] = true
		if _, err := tx.Exec(ctx, `INSERT INTO memory_erasure_targets
				(request_id, target_kind, target_id, document_id, content_digest, erased_at)
			VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), $6)`,
			id, t.kind, t.id, t.documentID, t.digest, erasedAt); err != nil {
			return domain.ErasureReceipt{}, mapError("Erase: tombstone", err)
		}
	}
	for _, c := range cids {
		if _, err := tx.Exec(ctx, `INSERT INTO memory_erasure_outbox (request_id, step, ref)
			VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, id, domain.ErasureStepCASDelete, c); err != nil {
			return domain.ErasureReceipt{}, mapError("Erase: outbox", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.ErasureReceipt{}, err
	}
	return domain.ErasureReceipt{
		RequestID: id, Principal: req.Principal, PrincipalKind: req.PrincipalKind, Reason: req.Reason,
		SelectorKinds: req.Selector.Kinds(), Status: status, ErasedAt: erasedAt.UTC(),
		Counts: run.counts, Targets: len(seen), PendingSteps: len(cids),
	}, nil
}

// eraseTx runs every deletion step of D4 inside run.tx. matched reports whether the
// selector reached any stored record.
func (s *PgErasureStore) eraseTx(ctx context.Context, run *erasureRun, sel domain.ErasureSelector) (bool, error) {
	tx := run.tx
	docSet := map[string]bool{}
	for _, d := range sel.DocumentIDs {
		docSet[d] = true
	}

	// Threads → the documents that arrived on them (upload associations, ADR-0138
	// B1.1, and the chunks' ingest thread, domain.MetaIngestThreadID).
	for _, th := range sel.ThreadIDs {
		docs, err := collectIDs(ctx, tx, `SELECT document_id FROM document_associations WHERE session_id = $1
			UNION SELECT document_id FROM `+TableChunks+`
			 WHERE metadata->>'`+domain.MetaIngestThreadID+`' = $1 AND document_id IS NOT NULL`, th)
		if err != nil {
			return false, fmt.Errorf("erase: resolve thread: %w", err)
		}
		if len(docs) > 0 {
			run.target(domain.ErasureTargetThread, th, "", "")
		}
		for _, d := range docs {
			docSet[d] = true
		}
	}

	// Chunks: a chunk that is its document's only content (besides the source-doc entity
	// stub) erases the document (D2); otherwise it is erased alone.
	var singleChunks []string
	for _, c := range sel.ChunkIDs {
		var doc *string
		err := tx.QueryRow(ctx, `SELECT document_id FROM `+TableChunks+` WHERE id = $1`, c).Scan(&doc)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("erase: resolve chunk: %w", err)
		}
		if doc == nil || *doc == "" {
			singleChunks = append(singleChunks, c)
			run.target(domain.ErasureTargetChunk, c, "", "")
			continue
		}
		var others int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+TableChunks+`
			WHERE document_id = $1 AND id <> $2 AND id NOT LIKE 'source_doc:%'`, *doc, c).Scan(&others); err != nil {
			return false, fmt.Errorf("erase: resolve chunk siblings: %w", err)
		}
		run.target(domain.ErasureTargetChunk, c, *doc, "")
		if others == 0 {
			docSet[*doc] = true
		} else {
			singleChunks = append(singleChunks, c)
		}
	}

	docs := make([]string, 0, len(docSet))
	for d := range docSet {
		docs = append(docs, d)
	}
	sort.Strings(docs)
	// D4.1: one advisory lock per document, in id order (no deadlock between two erases);
	// the S4 write fences take the same key.
	for _, d := range docs {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, d); err != nil {
			return false, fmt.Errorf("erase: lock document: %w", err)
		}
	}

	matched := false
	if len(docs) > 0 {
		m, err := s.eraseDocuments(ctx, run, docs)
		if err != nil {
			return false, err
		}
		matched = matched || m
	}
	if len(sel.ExperienceIDs) > 0 {
		m, err := s.eraseExperiences(ctx, run, sel.ExperienceIDs)
		if err != nil {
			return false, err
		}
		matched = matched || m
	}
	if len(singleChunks) > 0 {
		if err := s.eraseChunks(ctx, run, singleChunks); err != nil {
			return false, err
		}
		matched = true
	}
	for _, ref := range sel.EntityRefs {
		n, err := s.eraseItemsWhere(ctx, run,
			`ki.entity_id = $1 OR EXISTS (SELECT 1 FROM statement_values sv
				WHERE sv.item_id = ki.id AND sv.value_type = 'entity' AND sv.value_entity_id = $1)`,
			[]any{ref}, domain.KnowledgeErasure{Entities: []string{ref}, ValueEntityRef: ref})
		if err != nil {
			return false, err
		}
		if n > 0 {
			run.target(domain.ErasureTargetEntity, ref, "", "")
			matched = true
		}
	}
	return matched, nil
}

// eraseDocuments erases docs (sorted) and everything derived from them.
func (s *PgErasureStore) eraseDocuments(ctx context.Context, run *erasureRun, docs []string) (bool, error) {
	tx := run.tx
	// Which of them exist, and their content digests, read BEFORE anything is deleted.
	rows, err := tx.Query(ctx, `SELECT id, COALESCE(content_digest, '') FROM `+TableDocuments+` WHERE id = ANY($1)`, docs)
	if err != nil {
		return false, fmt.Errorf("erase: documents: %w", err)
	}
	digests := map[string]string{}
	for rows.Next() {
		var id, dg string
		if err := rows.Scan(&id, &dg); err != nil {
			rows.Close()
			return false, err
		}
		digests[id] = dg
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	hasLegacy, err := tableExists(ctx, tx, "documents_legacy")
	if err != nil {
		return false, err
	}
	legacyIDs := map[string]bool{}
	if hasLegacy {
		ids, err := collectIDs(ctx, tx, `SELECT DISTINCT d FROM unnest($1::text[]) d
			WHERE EXISTS (SELECT 1 FROM documents_legacy l WHERE l.id = d OR l.metadata->>'document_id' = d)`, docs)
		if err != nil {
			return false, fmt.Errorf("erase: documents_legacy ids: %w", err)
		}
		for _, d := range ids {
			legacyIDs[d] = true
		}
	}
	// ADR-0105: a document's evidence is keyed by its document id (captureEvidence).
	evRows, err := tx.Query(ctx, `SELECT id, source_key, content_hash FROM evidence WHERE source_key = ANY($1) ORDER BY id`, docs)
	if err != nil {
		return false, fmt.Errorf("erase: evidence ids: %w", err)
	}
	var evidenceIDs []string
	evidenceDoc := map[string]string{}
	for evRows.Next() {
		var id, key, hash string
		if err := evRows.Scan(&id, &key, &hash); err != nil {
			evRows.Close()
			return false, err
		}
		evidenceIDs = append(evidenceIDs, id)
		evidenceDoc[id] = key
		if hash != "" {
			run.cids[hash] = true
		}
	}
	evRows.Close()
	if err := evRows.Err(); err != nil {
		return false, err
	}
	// The source-doc body in the CAS (mintSourceDoc's content_cid on the entity stub).
	cids, err := collectIDs(ctx, tx, `SELECT DISTINCT metadata->>'content_cid' FROM `+TableChunks+`
		WHERE document_id = ANY($1) AND COALESCE(metadata->>'content_cid', '') <> ''`, docs)
	if err != nil {
		return false, fmt.Errorf("erase: source-doc cids: %w", err)
	}
	for _, c := range cids {
		run.cids[c] = true
	}

	hasEvidence := map[string]bool{}
	for _, ev := range evidenceIDs {
		hasEvidence[evidenceDoc[ev]] = true
	}
	found := false
	for _, d := range docs {
		_, isDoc := digests[d]
		if isDoc || legacyIDs[d] || hasEvidence[d] {
			found = true
			run.target(domain.ErasureTargetDocument, d, d, digests[d])
		}
	}
	if !found {
		return false, nil
	}
	for _, ev := range evidenceIDs {
		run.target(domain.ErasureTargetEvidence, ev, evidenceDoc[ev], "")
	}

	// D4.3–5: the ADR-0138 cascade (fact items by evidence or source_document value,
	// markers, journal, triplets, PageRank, edges, sections, chunks incl. the entity stub).
	counts, err := s.l.retireDocumentsTx(ctx, run.tx, docs)
	if err != nil {
		return false, fmt.Errorf("erase: %w", err)
	}
	for k, v := range counts {
		run.counts[k] += v
	}
	// The documents rows (document_associations cascade), their content-free version
	// history, and ADR-0093's frozen pre-split copies.
	if err := run.exec(ctx, eraseDocuments, `DELETE FROM `+TableDocuments+` WHERE id = ANY($1)`, docs); err != nil {
		return false, err
	}
	if err := run.exec(ctx, eraseDocumentVersions, `DELETE FROM document_versions WHERE document_id = ANY($1)`, docs); err != nil {
		return false, err
	}
	if hasLegacy {
		if err := run.exec(ctx, eraseDocumentsLegacy,
			`DELETE FROM documents_legacy WHERE id = ANY($1) OR metadata->>'document_id' = ANY($1)`, docs); err != nil {
			return false, err
		}
	}
	if len(evidenceIDs) > 0 {
		if err := s.eraseEvidence(ctx, run, evidenceIDs); err != nil {
			return false, err
		}
	}
	return true, nil
}

// eraseEvidence deletes evidence rows and their dependents in FK order (D4.8): every
// knowledge item derived from them (any kind, through EraseItemsTx), events (event_roles
// cascade), observations and links by evidence_id, the outbox rows, then the evidence.
// entities.first_seen_evidence and evidence.revises_id become NULL by FK action (0051).
func (s *PgErasureStore) eraseEvidence(ctx context.Context, run *erasureRun, evidenceIDs []string) error {
	if _, err := s.eraseItemsWhere(ctx, run, `ki.evidence_id = ANY($1)`, []any{evidenceIDs},
		domain.KnowledgeErasure{EvidenceIDs: evidenceIDs}); err != nil {
		return err
	}
	for _, st := range []struct{ store, sql string }{
		{eraseEvents, `DELETE FROM events WHERE evidence_id = ANY($1)`},
		{eraseObservations, `DELETE FROM observations WHERE evidence_id = ANY($1)`},
		{eraseLinks, `DELETE FROM links WHERE evidence_id = ANY($1)`},
		{eraseEvidenceOutbox, `DELETE FROM evidence_outbox WHERE evidence_id = ANY($1)`},
		{eraseEvidence, `DELETE FROM evidence WHERE id = ANY($1)`},
	} {
		if err := run.exec(ctx, st.store, st.sql, evidenceIDs); err != nil {
			return err
		}
	}
	return nil
}

// eraseExperiences deletes ADR-0095 episode parents and their chunks' derivatives.
func (s *PgErasureStore) eraseExperiences(ctx context.Context, run *erasureRun, ids []string) (bool, error) {
	found, err := collectIDs(ctx, run.tx, `SELECT id FROM `+TableExperiences+` WHERE id = ANY($1) ORDER BY id`, ids)
	if err != nil {
		return false, fmt.Errorf("erase: experiences: %w", err)
	}
	if len(found) == 0 {
		return false, nil
	}
	chunkIDs, err := collectIDs(ctx, run.tx, `SELECT id FROM `+TableChunks+` WHERE experience_id = ANY($1)`, found)
	if err != nil {
		return false, fmt.Errorf("erase: experience chunks: %w", err)
	}
	if len(chunkIDs) > 0 {
		if err := s.eraseChunks(ctx, run, chunkIDs); err != nil {
			return false, err
		}
	}
	for _, e := range found {
		run.target(domain.ErasureTargetExperience, e, "", "")
	}
	// experience_derivations cascade from experiences.
	return true, run.exec(ctx, eraseExperiences, `DELETE FROM `+TableExperiences+` WHERE id = ANY($1)`, found)
}

// eraseChunks erases single chunks and their derivatives: fact items keyed into them
// (source_chunk value), KG triplets and PageRank (deleted explicitly so they are counted;
// the 0051 FK would cascade them), structure/Hebbian edges, then the chunks.
func (s *PgErasureStore) eraseChunks(ctx context.Context, run *erasureRun, chunkIDs []string) error {
	if _, err := s.eraseItemsWhere(ctx, run,
		`EXISTS (SELECT 1 FROM statement_values sv WHERE sv.item_id = ki.id AND sv.predicate = $2 AND sv.value_text = ANY($1))`,
		[]any{chunkIDs, domain.FactPredSourceChunk},
		domain.KnowledgeErasure{ValuePredicate: domain.FactPredSourceChunk, ValueTexts: chunkIDs}); err != nil {
		return err
	}
	for _, st := range []struct{ store, sql string }{
		{cascadeTriplets, `DELETE FROM chunk_triplets WHERE chunk_id = ANY($1)`},
		{cascadePageRank, `DELETE FROM chunk_pagerank WHERE chunk_id = ANY($1)`},
		{cascadeEdges, `DELETE FROM document_edges WHERE source_id = ANY($1) OR target_id = ANY($1)`},
		{cascadeChunks, `DELETE FROM ` + TableChunks + ` WHERE id = ANY($1)`},
	} {
		if err := run.exec(ctx, st.store, st.sql, chunkIDs); err != nil {
			return err
		}
	}
	return nil
}

// eraseItemsWhere erases, through EraseItemsTx, the knowledge items matching where (an
// SQL condition over alias ki) in every (namespace, kind) that has any, in a fixed
// order. sel must select the same items for EraseItemsTx.
func (s *PgErasureStore) eraseItemsWhere(ctx context.Context, run *erasureRun, where string, args []any, sel domain.KnowledgeErasure) (int, error) {
	rows, err := run.tx.Query(ctx, `SELECT DISTINCT ki.namespace_id, ki.kind FROM knowledge_items ki
		WHERE `+where+` ORDER BY 1, 2`, args...)
	if err != nil {
		return 0, fmt.Errorf("erase: knowledge item scopes: %w", err)
	}
	var scopes [][2]string
	for rows.Next() {
		var ns, kind string
		if err := rows.Scan(&ns, &kind); err != nil {
			rows.Close()
			return 0, err
		}
		scopes = append(scopes, [2]string{ns, kind})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	total := 0
	for _, sc := range scopes {
		n, err := s.l.k.EraseItemsTx(ctx, run.tx, sc[0], sc[1], sel)
		if err != nil {
			return 0, fmt.Errorf("erase: knowledge items (%s/%s): %w", sc[0], sc[1], err)
		}
		run.counts[cascadeFactItems] += n
		total += n
	}
	return total, nil
}

// earlierErasure returns the latest request that erased one of the selector's ids
// (for NothingToEraseError), or "".
func earlierErasure(ctx context.Context, q pgxQuerier, sel domain.ErasureSelector) (string, error) {
	var ids []string
	for _, l := range [][]string{sel.DocumentIDs, sel.ThreadIDs, sel.ChunkIDs, sel.ExperienceIDs, sel.EntityRefs} {
		ids = append(ids, l...)
	}
	var id string
	err := q.QueryRow(ctx, `SELECT request_id FROM memory_erasure_targets
		WHERE target_id = ANY($1) ORDER BY erased_at DESC, request_id LIMIT 1`, ids).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", mapError("Erase: earlier request", err)
	}
	return id, nil
}

// Receipt implements domain.MemoryEraser.
func (s *PgErasureStore) Receipt(ctx context.Context, requestID string) (domain.ErasureReceipt, bool, error) {
	return receiptFrom(ctx, s.l.k.pool, requestID)
}

func receiptFrom(ctx context.Context, q pgxQuerier, requestID string) (domain.ErasureReceipt, bool, error) {
	var (
		rec      domain.ErasureReceipt
		kind     string
		reason   string
		deadline *time.Time
		counts   []byte
	)
	err := q.QueryRow(ctx, `SELECT e.request_id, e.principal, e.principal_kind, e.reason_code, e.selector_kinds,
			e.status, e.erased_at, e.async_deadline, e.counts,
			(SELECT count(*) FROM memory_erasure_targets t WHERE t.request_id = e.request_id),
			(SELECT count(*) FROM memory_erasure_outbox o WHERE o.request_id = e.request_id AND o.processed_at IS NULL)
		FROM memory_erasures e WHERE e.request_id = $1`, requestID).
		Scan(&rec.RequestID, &rec.Principal, &kind, &reason, &rec.SelectorKinds, &rec.Status, &rec.ErasedAt,
			&deadline, &counts, &rec.Targets, &rec.PendingSteps)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErasureReceipt{}, false, nil
	}
	if err != nil {
		return domain.ErasureReceipt{}, false, mapError("Receipt", err)
	}
	rec.PrincipalKind = domain.ErasurePrincipalKind(kind)
	rec.Reason = domain.ErasureReason(reason)
	rec.ErasedAt = rec.ErasedAt.UTC()
	if deadline != nil {
		rec.AsyncDeadline = deadline.UTC()
	}
	rec.Counts = map[string]int{}
	if err := json.Unmarshal(counts, &rec.Counts); err != nil {
		return domain.ErasureReceipt{}, false, fmt.Errorf("receipt %s: counts: %w", requestID, err)
	}
	return rec, true, nil
}

func tableExists(ctx context.Context, q pgxQuerier, name string) (bool, error) {
	var ok bool
	if err := q.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&ok); err != nil {
		return false, err
	}
	return ok, nil
}
