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

// PgDocumentLifecycle is the document identity check and version chain of ADR-0138
// (D2–D4) over the one kernel database.
//
// It holds the knowledge store rather than a bare pool because a document's derived
// fact items are erased through PgKnowledgeStore.EraseItemsTx — the substrate's one
// erasure, with its assessment cleanup, re-derivation and ADR-0137 Q5 sentinels — inside
// the SAME transaction that deletes the document's chunks, KG triplets and structure
// (AGENTS.md §4.3: the existing erasure is extended, not re-implemented here).
type PgDocumentLifecycle struct {
	k *PgKnowledgeStore
}

// NewPgDocumentLifecycle builds the lifecycle over the knowledge store's pool.
func NewPgDocumentLifecycle(k *PgKnowledgeStore) *PgDocumentLifecycle {
	return &PgDocumentLifecycle{k: k}
}

var _ domain.DocumentLifecycleStore = (*PgDocumentLifecycle)(nil)

// Tables the document cascade reports counts for (DocumentCascadeCounts keys).
const (
	cascadeFactItems         = "knowledge_items"
	cascadeMarkers           = "knowledge_extractions"
	cascadeExtractionJournal = "knowledge_extraction_windows"
	cascadeTriplets          = "chunk_triplets"
	cascadePageRank          = "chunk_pagerank"
	cascadeEdges             = "document_edges"
	cascadeSections          = "document_sections"
	cascadeChunks            = "chunks"
)

// writerOfRecordSQL is the writer of record of document d: documents.written_by, else the
// source_agent_id its chunks carry (rows written before 0026 recorded the writer only
// there, ingestion_chunks.go chunkMetadata), else ”. The entity stub is excluded: its
// metadata names an "author", not a source_agent_id.
const writerOfRecordSQL = `COALESCE(NULLIF(d.written_by, ''),
	(SELECT c.metadata->>'source_agent_id' FROM ` + TableChunks + ` c
	  WHERE c.document_id = d.id AND c.metadata->>'source_agent_id' IS NOT NULL LIMIT 1), '')`

// DocumentRecord implements domain.DocumentLifecycleStore.
func (l *PgDocumentLifecycle) DocumentRecord(ctx context.Context, id string) (domain.DocumentRecord, error) {
	rec := domain.DocumentRecord{ID: id}
	if id == "" {
		return rec, nil
	}
	var digest *string
	err := l.k.pool.QueryRow(ctx, `SELECT `+writerOfRecordSQL+`, d.content_digest, d.version,
			d.title, d.created_at, d.updated_at
		FROM `+TableDocuments+` d WHERE d.id = $1`, id).
		Scan(&rec.WrittenBy, &digest, &rec.Version, &rec.Title, &rec.CreatedAt, &rec.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return rec, nil
	}
	if err != nil {
		return rec, mapError("DocumentRecord", err)
	}
	rec.Exists = true
	if digest != nil {
		rec.ContentDigest = *digest
	}
	return rec, nil
}

// FindByContent implements domain.DocumentLifecycleStore (ADR-0138 B1.1).
//
// Served by idx_documents_writer_digest (built concurrently, migrate.concurrentIndexes).
// A row must still hold content (a chunk) to count: a documents row whose chunk write
// failed would otherwise turn every later upload of the same bytes into a no-op against
// nothing. The oldest match wins, ties by id, so the answer is deterministic.
func (l *PgDocumentLifecycle) FindByContent(ctx context.Context, writer, digest string) (domain.DocumentRecord, error) {
	rec := domain.DocumentRecord{}
	if writer == "" || digest == "" {
		return rec, nil
	}
	err := l.k.pool.QueryRow(ctx, `SELECT d.id, d.written_by, d.content_digest, d.version,
			d.title, d.created_at, d.updated_at
		FROM `+TableDocuments+` d
		WHERE d.written_by = $1 AND d.content_digest = $2
		  AND EXISTS (SELECT 1 FROM `+TableChunks+` c WHERE c.document_id = d.id)
		ORDER BY d.created_at, d.id LIMIT 1`, writer, digest).
		Scan(&rec.ID, &rec.WrittenBy, &rec.ContentDigest, &rec.Version, &rec.Title, &rec.CreatedAt, &rec.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.DocumentRecord{}, nil
	}
	if err != nil {
		return domain.DocumentRecord{}, mapError("FindByContent", err)
	}
	rec.Exists = true
	return rec, nil
}

// RecordAssociation implements domain.DocumentLifecycleStore (ADR-0138 B1.1): one row
// per (document, writer, session, scope), keyed naturally so a retry or a repeat upload
// updates the same row (AGENTS.md §2.6) — last_seen_at moves and deliveries counts.
// The FK cascades from documents, so ADR-0137's forget removes these rows with the
// document.
func (l *PgDocumentLifecycle) RecordAssociation(ctx context.Context, a domain.DocumentAssociation) error {
	if a.DocumentID == "" {
		return nil
	}
	scope := a.Scope
	if scope == nil {
		scope = []string{}
	}
	_, err := l.k.pool.Exec(ctx, `INSERT INTO document_associations
			(document_id, writer, session_id, scope, upload_name)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (document_id, writer, session_id, scope) DO UPDATE SET
			upload_name  = EXCLUDED.upload_name,
			last_seen_at = now(),
			deliveries   = document_associations.deliveries + 1`,
		a.DocumentID, a.Writer, a.SessionID, scope, a.UploadName)
	if err != nil {
		return mapError("RecordAssociation", err)
	}
	return nil
}

// ReviseDocument implements domain.DocumentLifecycleStore (ADR-0138 D3).
//
// The document row is locked FOR UPDATE, so two concurrent ingests of one id serialize
// here: the second sees the first's digest and either retires it or finds it unchanged.
// Retirement runs BEFORE the caller saves the new chunks, because the KG triplet batcher
// and the fact extractor are asynchronous and keyed by chunk id — deleting after the
// save would race them and delete the new version's derivatives.
func (l *PgDocumentLifecycle) ReviseDocument(ctx context.Context, rev domain.DocumentRevision) (domain.DocumentRevisionOutcome, error) {
	out := domain.DocumentRevisionOutcome{Previous: domain.DocumentRecord{ID: rev.ID}, Version: 1}
	if rev.ID == "" {
		return out, nil
	}
	tx, err := l.k.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after a successful commit

	var (
		digest  *string
		text    string
		created *time.Time
	)
	err = tx.QueryRow(ctx, `SELECT `+writerOfRecordSQL+`, d.content_digest, d.version, d.text, d.created_at
		FROM `+TableDocuments+` d WHERE d.id = $1 FOR UPDATE OF d`, rev.ID).
		Scan(&out.Previous.WrittenBy, &digest, &out.Previous.Version, &text, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil // a new document: the ingest inserts version 1
	}
	if err != nil {
		return out, mapError("ReviseDocument", err)
	}
	out.Previous.Exists = true
	if digest != nil {
		out.Previous.ContentDigest = *digest
	}
	out.Version = out.Previous.Version

	// ADR-0138 Q1: a recorded writer is never taken over by another. A writer-less row
	// (kernel-internal, or written before any writer was recorded) is not protected, and a
	// writer-less ingest neither claims nor is refused.
	if w := out.Previous.WrittenBy; w != "" && rev.Writer != "" && w != rev.Writer {
		return out, fmt.Errorf("revise document %q: %w", rev.ID, domain.ErrDocumentOwnedByAnotherWriter)
	}

	var changed bool
	if out.Previous.ContentDigest != "" {
		changed = out.Previous.ContentDigest != rev.ContentDigest
	} else {
		// A legacy row (before 0026) has no digest: equal text and no binary payload is the
		// same content; a binary cannot be proven equal, so it is re-ingested afresh.
		changed = rev.HasData || text != rev.Text
	}
	if !changed {
		return out, tx.Commit(ctx)
	}

	counts, err := l.retireDocumentsTx(ctx, tx, []string{rev.ID})
	if err != nil {
		return out, fmt.Errorf("revise document %q: retire version %d: %w", rev.ID, out.Previous.Version, err)
	}
	cj, _ := json.Marshal(counts) // map[string]int always marshals
	var createdAt any
	if created != nil {
		createdAt = created.UTC()
	}
	if _, err := tx.Exec(ctx, `INSERT INTO document_versions
			(document_id, version, content_digest, written_by, created_at, counts)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb)
		ON CONFLICT (document_id, version) DO NOTHING`,
		rev.ID, out.Previous.Version, out.Previous.ContentDigest, out.Previous.WrittenBy, createdAt, string(cj)); err != nil {
		return out, mapError("ReviseDocument: version row", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE `+TableDocuments+`
		SET version = version + 1, content_digest = $2,
		    written_by = COALESCE(NULLIF($3, ''), written_by)
		WHERE id = $1`, rev.ID, rev.ContentDigest, rev.Writer); err != nil {
		return out, mapError("ReviseDocument: advance version", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return out, err
	}
	out.Retired = true
	out.Version = out.Previous.Version + 1
	out.Counts = counts
	return out, nil
}

// retireDocumentsTx is the ADR-0138 D4 document cascade, in the caller's transaction:
// everything derived from the documents' content, then their sections and chunks. The
// documents rows stay (they carry the version chain). ADR-0137's forget is to compose
// this same cascade inside its D4 transaction and delete the documents rows (plus
// tombstones, evidence and the remaining participants) around it — it is not built yet,
// so no caller deletes a documents row here.
//
// Order matters only where one step reads what a later one deletes: the chunk and
// section ids are collected first, fact items are erased while their source chunks still
// exist (EraseItemsTx re-derives surviving keys), and chunks go last.
func (l *PgDocumentLifecycle) retireDocumentsTx(ctx context.Context, tx pgx.Tx, docIDs []string) (domain.DocumentCascadeCounts, error) {
	counts := domain.DocumentCascadeCounts{}
	if len(docIDs) == 0 {
		return counts, nil
	}
	chunkIDs, err := collectIDs(ctx, tx, `SELECT id FROM `+TableChunks+` WHERE document_id = ANY($1)`, docIDs)
	if err != nil {
		return nil, fmt.Errorf("document cascade: chunk ids: %w", err)
	}
	sectionIDs, err := collectIDs(ctx, tx, `SELECT id FROM document_sections WHERE document_id = ANY($1)`, docIDs)
	if err != nil {
		return nil, fmt.Errorf("document cascade: section ids: %w", err)
	}
	// ADR-0105: evidence of a document is keyed by its document id (captureEvidence
	// SourceKey). The evidence rows themselves are NOT deleted here (D4: on re-ingest they
	// are the immutable record of each delivered revision).
	evidenceIDs, err := collectIDs(ctx, tx, `SELECT id FROM evidence WHERE source_key = ANY($1)`, docIDs)
	if err != nil {
		return nil, fmt.Errorf("document cascade: evidence ids: %w", err)
	}

	// 1. memory_fact items derived from the documents, in every namespace that has any,
	//    through the substrate's own erasure (assessments, resolutions, sentinels,
	//    re-derivation). Selected by their source_document value (works with evidence
	//    capture off, the default) or their evidence row.
	sel := domain.KnowledgeErasure{
		EvidenceIDs:    evidenceIDs,
		ValuePredicate: domain.FactPredSourceDocument,
		ValueTexts:     docIDs,
	}
	namespaces, err := collectIDs(ctx, tx, `SELECT DISTINCT ki.namespace_id FROM knowledge_items ki
		WHERE ki.kind = $2 AND (ki.evidence_id = ANY($3) OR EXISTS (
			SELECT 1 FROM statement_values sv WHERE sv.item_id = ki.id
			  AND sv.predicate = $4 AND sv.value_text = ANY($1)))`,
		docIDs, domain.KindMemoryFact, nonNil(evidenceIDs), domain.FactPredSourceDocument)
	if err != nil {
		return nil, fmt.Errorf("document cascade: fact namespaces: %w", err)
	}
	sort.Strings(namespaces)
	for _, ns := range namespaces {
		n, err := l.k.EraseItemsTx(ctx, tx, ns, domain.KindMemoryFact, sel)
		if err != nil {
			return nil, fmt.Errorf("document cascade: erase fact items (%s): %w", ns, err)
		}
		counts[cascadeFactItems] += n
	}

	// 2. Extraction markers and the two-phase journal: without this a re-ingest of the
	//    same chunk ids would find the marker and SKIP extraction of the new text
	//    (T-20261103-fact-extraction-marker-hashes-chunk-ids-only-so).
	steps := []struct {
		table string
		sql   string
		arg   []string
	}{
		{cascadeMarkers, `DELETE FROM knowledge_extractions WHERE document_id = ANY($1)`, docIDs},
		{cascadeExtractionJournal, `DELETE FROM knowledge_extraction_windows WHERE document_id = ANY($1)`, docIDs},
		// 3. KG triplets and PageRank have no FK to chunks (ADR-0137 observation); they are
		//    deleted by chunk id.
		{cascadeTriplets, `DELETE FROM chunk_triplets WHERE chunk_id = ANY($1)`, chunkIDs},
		{cascadePageRank, `DELETE FROM chunk_pagerank WHERE chunk_id = ANY($1)`, chunkIDs},
		// 4. Structure and Hebbian edges touching any chunk, section or the document itself
		//    (the target side has no FK, 0001/0009).
		{cascadeEdges, `DELETE FROM document_edges WHERE source_id = ANY($1) OR target_id = ANY($1)`,
			append(append(append([]string(nil), chunkIDs...), sectionIDs...), docIDs...)},
	}
	// 5. The sections and chunks themselves (chunk_embeddings cascade from chunks). This
	//    includes the source-doc entity stub, whose document_id is this document; the
	//    ingest re-mints it after the new chunks.
	steps = append(steps,
		struct {
			table string
			sql   string
			arg   []string
		}{cascadeSections, `DELETE FROM document_sections WHERE document_id = ANY($1)`, docIDs},
		struct {
			table string
			sql   string
			arg   []string
		}{cascadeChunks, `DELETE FROM ` + TableChunks + ` WHERE document_id = ANY($1)`, docIDs})
	for _, st := range steps {
		if len(st.arg) == 0 {
			continue
		}
		tag, err := tx.Exec(ctx, st.sql, st.arg)
		if err != nil {
			return nil, fmt.Errorf("document cascade: %s: %w", st.table, err)
		}
		if n := int(tag.RowsAffected()); n > 0 {
			counts[st.table] += n
		}
	}
	return counts, nil
}

func collectIDs(ctx context.Context, q pgxQuerier, sql string, args ...any) ([]string, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
