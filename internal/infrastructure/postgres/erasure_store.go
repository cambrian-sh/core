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
	"github.com/cambrian-sh/core/internal/objects"
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
// it unexported (kernel.MemoryStack) and hands principals only the authz.EnforcingEraser
// that wraps it (ADR-0137 S3, ENG-411 stage B). It is also the domain.ErasureOutbox the
// erasure worker drains (S5).
//
// The ADR-0149 object rows (objects_<kind>) of every entity an erased observation
// described are recomputed inside the transaction (ENG-1858); knowledge items recompute
// theirs through EraseItemsTx's re-derivation.
//
// Chat replies that rested on an erased record (conversation_messages.source_refs) are
// REDACTED in place, not deleted — text replaced with domain.RedactedReplyText, the row
// kept so the transcript still reads in order — and listed on the receipt (ENG-2109,
// redactReplies).
//
// Plugin rows that reference the erased evidence (premium ingress studio) are erased by
// the domain.ErasureParticipant hooks INSIDE the same transaction, before the evidence
// rows (ADR-0137 D4 step 8, S7). Without a participant for them such a row still fails
// the evidence FK and the whole erase rolls back, erasing nothing. After commit every
// domain.ErasureObserver receives the receipt (the premium signed ADR-0103 receipt, D8).
type PgErasureStore struct {
	l *PgDocumentLifecycle
	// asyncDeadline is how long the async tail of a request may take (execution.erasure
	// .async_deadline_seconds); 0 leaves memory_erasures.async_deadline NULL.
	asyncDeadline time.Duration
	// participants erase plugin rows keyed by evidence ids inside the transaction.
	participants []domain.ErasureParticipant
	// observer receives each executed (non-replayed) receipt after commit.
	observer domain.ErasureObserver
	// tailSteps are the plugin async steps (domain.ErasureTailStep names) queued once
	// per executed request (ADR-0137 D7, ENG-1966).
	tailSteps []string
}

// ErasureStoreOption configures a PgErasureStore.
type ErasureStoreOption func(*PgErasureStore)

// WithErasureAsyncDeadline sets the deadline stamped on a request with async steps
// (ADR-0137 D7): past it the request reads overdue on the operator plane.
func WithErasureAsyncDeadline(d time.Duration) ErasureStoreOption {
	return func(s *PgErasureStore) { s.asyncDeadline = d }
}

// WithErasureParticipants adds plugin participants (app.Registry.AddErasureParticipant)
// that erase their own evidence-keyed rows inside the erase transaction (ADR-0137 S7).
func WithErasureParticipants(ps ...domain.ErasureParticipant) ErasureStoreOption {
	return func(s *PgErasureStore) {
		for _, p := range ps {
			if p != nil {
				s.participants = append(s.participants, p)
			}
		}
	}
}

// WithErasureTailSteps queues one outbox row per executed request (ref = the request
// id) for every named plugin step (app.Registry.AddErasureTailStep; ADR-0137 D7): the
// erasure worker runs the step with what the request erased. Empty names are skipped.
func WithErasureTailSteps(steps ...string) ErasureStoreOption {
	return func(s *PgErasureStore) {
		for _, st := range steps {
			if st != "" {
				s.tailSteps = append(s.tailSteps, st)
			}
		}
	}
}

// WithErasureObserver sets the observer told of every executed erasure after commit
// (app.Registry.AddErasureObserver fans several into one; ADR-0137 D8).
func WithErasureObserver(o domain.ErasureObserver) ErasureStoreOption {
	return func(s *PgErasureStore) { s.observer = o }
}

// NewPgErasureStore builds the eraser over the document lifecycle (and through it the
// knowledge store and its pool).
func NewPgErasureStore(l *PgDocumentLifecycle, opts ...ErasureStoreOption) *PgErasureStore {
	if l == nil || l.k == nil {
		panic("postgres.NewPgErasureStore: nil document lifecycle")
	}
	s := &PgErasureStore{l: l}
	for _, o := range opts {
		o(s)
	}
	return s
}

var (
	_ domain.MemoryEraser  = (*PgErasureStore)(nil)
	_ domain.ErasureOutbox = (*PgErasureStore)(nil)
)

// Store names the erasure counts use beyond the document cascade's (cascade* constants).
const (
	eraseDocuments        = "documents"
	eraseDocumentVersions = "document_versions"
	eraseDerivationJobs   = TableDerivationJobs
	eraseDocumentsLegacy  = "documents_legacy"
	eraseExperiences      = "experiences"
	eraseEvidence         = "evidence"
	eraseEvidenceOutbox   = "evidence_outbox"
	eraseEvents           = "events"
	eraseObservations     = "observations"
	eraseLinks            = "links"
	eraseLedgerEntries    = "ledger_entries"
	// eraseConversationMessages counts agent replies REDACTED in place (ENG-2109),
	// not deleted: the transcript keeps the row.
	eraseConversationMessages = "conversation_messages"
	// eraseConversationSummaries counts the chat lane's rolling conversation summaries
	// (domain.LedgerToolConversationSummary ledger entries) deleted because they
	// condensed a reply resting on an erased record (ENG-2155). They are ledger entries,
	// so ledger_entries counts them too; this names them apart on the receipt.
	eraseConversationSummaries = "conversation_summaries"
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
	// bodies are artifact-vault hashes of erased ledger entries (ADR-0157 D5.6);
	// the worker deletes each once no surviving ledger row names it.
	bodies map[string]bool
	// requestID is the erase request's id: the writer of record of the revision a
	// memory-notes erase writes (content-free, and it names the receipt).
	requestID string
	// ledgerChecked/hasLedger cache whether ledger_entries exists (a kernel
	// with the ledger off may never have migrated past 0034… it always has).
	ledgerChecked, hasLedger bool
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

	run := &erasureRun{tx: tx, counts: map[string]int{}, cids: map[string]bool{}, bodies: map[string]bool{}, requestID: id}
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
	bodies := make([]string, 0, len(run.bodies))
	for h := range run.bodies {
		bodies = append(bodies, h)
	}
	sort.Strings(bodies)
	status := domain.ErasureStatusComplete
	if len(cids)+len(bodies)+len(s.tailSteps) > 0 {
		status = domain.ErasureStatusPendingAsync
	}
	cj, _ := json.Marshal(run.counts) // map[string]int always marshals
	var erasedAt time.Time
	var deadline *time.Time
	if err := tx.QueryRow(ctx, `INSERT INTO memory_erasures
			(request_id, principal, principal_kind, reason_code, selector_kinds, status, counts, completed_at, async_deadline)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, CASE WHEN $6 = 'complete' THEN now() END,
			CASE WHEN $6 = 'pending_async' AND $8::bigint > 0 THEN now() + make_interval(secs => $8::bigint) END)
		RETURNING erased_at, async_deadline`,
		id, req.Principal, string(req.PrincipalKind), string(req.Reason), req.Selector.Kinds(), status, string(cj),
		int64(s.asyncDeadline/time.Second)).
		Scan(&erasedAt, &deadline); err != nil {
		return domain.ErasureReceipt{}, mapError("Erase: receipt", err)
	}
	seen := map[[2]string]bool{}
	var redacted []string
	for _, t := range run.targets {
		k := [2]string{t.kind, t.id}
		if seen[k] {
			continue
		}
		seen[k] = true
		if t.kind == domain.ErasureTargetConversationMessage {
			redacted = append(redacted, t.id)
		}
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
	for _, h := range bodies {
		if _, err := tx.Exec(ctx, `INSERT INTO memory_erasure_outbox (request_id, step, ref)
			VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, id, domain.ErasureStepVaultDelete, h); err != nil {
			return domain.ErasureReceipt{}, mapError("Erase: outbox", err)
		}
	}
	// ADR-0137 D7 (ENG-1966): every plugin tail step runs once per request, over what
	// the request erased (the worker reads it back by the request id).
	for _, st := range s.tailSteps {
		if _, err := tx.Exec(ctx, `INSERT INTO memory_erasure_outbox (request_id, step, ref)
			VALUES ($1, $2, $1) ON CONFLICT DO NOTHING`, id, st); err != nil {
			return domain.ErasureReceipt{}, mapError("Erase: outbox", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.ErasureReceipt{}, err
	}
	rec := domain.ErasureReceipt{
		RequestID: id, Principal: req.Principal, PrincipalKind: req.PrincipalKind, Reason: req.Reason,
		SelectorKinds: req.Selector.Kinds(), Status: status, ErasedAt: erasedAt.UTC(),
		Counts: run.counts, Targets: len(seen), PendingSteps: len(cids) + len(bodies) + len(s.tailSteps),
	}
	if len(redacted) > 0 {
		sort.Strings(redacted)
		rec.RedactedMessages = redacted
	}
	if deadline != nil {
		rec.AsyncDeadline = deadline.UTC()
	}
	// D8: after commit, never inside the transaction — an observer cannot make an erase
	// fail or roll back, and a replay (above) is not observed again.
	if s.observer != nil {
		s.observer.ObserveErasure(context.WithoutCancel(ctx), rec)
	}
	return rec, nil
}

// erasureTx adapts the run's pgx transaction to the driver-free domain.ErasureTx a
// participant receives.
type erasureTx struct{ tx pgx.Tx }

func (t erasureTx) Exec(ctx context.Context, sql string, args ...any) (int64, error) {
	tag, err := t.tx.Exec(ctx, sql, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// eraseTx runs every deletion step of D4 inside run.tx. matched reports whether the
// selector reached any stored record.
func (s *PgErasureStore) eraseTx(ctx context.Context, run *erasureRun, sel domain.ErasureSelector) (bool, error) {
	tx := run.tx
	docSet := map[string]bool{}
	// A source-doc entity id names its document rows (ENG-2245, the listing's
	// resolution) — for a caller that skipped Resolve (the kernel's own erasures);
	// the enforcing eraser already hands Erase row ids.
	docIDs, _, err := expandEntityIDs(ctx, tx, sel.DocumentIDs)
	if err != nil {
		return false, fmt.Errorf("erase: resolve source-doc entity: %w", err)
	}
	for _, d := range docIDs {
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

	// Chunks: a chunk that belongs to a document WIDENS to the whole document (D2 as
	// amended by the owner, 2026-11-17, ENG-1947). Erasing one chunk of a multi-chunk
	// document alone would leave its words in documents.text, which the document-level
	// readers (operator GetDocument, MCP get_document / kernelDocumentReader) serve —
	// R5 broken. Rejected: redacting the chunk's span out of documents.text (partial-
	// record redaction is a v1 non-goal) and refusing chunk selectors on multi-chunk
	// documents. Resolve answers such a chunk as its document, so a preview shows what
	// will go. A document-less chunk (a mnemonic fact, an episode chunk) is erased alone.
	// (chunks.document_id references documents(id), so the document row exists.)
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
		run.target(domain.ErasureTargetChunk, c, *doc, "")
		docSet[*doc] = true
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
	// ENG-2106: entries of principal memory notes (erasure_memory_notes.go).
	if len(sel.MemoryNoteIDs) > 0 {
		m, err := s.eraseMemoryNotes(ctx, run, sel.MemoryNoteIDs, run.requestID)
		if err != nil {
			return false, err
		}
		matched = matched || m
	}
	// ENG-2147: a person's whole personal notes (the subject-wide compliance erase),
	// after the single entries so a request naming both tombstones each line once.
	if len(sel.MemoryNoteOwners) > 0 {
		m, err := s.eraseMemoryNoteOwners(ctx, run, sel.MemoryNoteOwners, run.requestID)
		if err != nil {
			return false, err
		}
		matched = matched || m
		// ENG-2254: and the person's tool-call arguments (erasure_tool_args.go).
		m, err = s.eraseToolArgsOf(ctx, run, sel.MemoryNoteOwners)
		if err != nil {
			return false, err
		}
		matched = matched || m
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

	// ADR-0157 D5.6: run-ledger entries that quote these documents (a recall's
	// hits name chunk and document ids) and everything derived from them, and the
	// chat replies built from them (ENG-2109, redacted in place). The
	// chunk ids are read before the cascade deletes the chunks.
	docChunks, err := collectIDs(ctx, tx, `SELECT id FROM `+TableChunks+` WHERE document_id = ANY($1)`, docs)
	if err != nil {
		return false, fmt.Errorf("erase: document chunks: %w", err)
	}
	if err := s.eraseQuoters(ctx, run, append(append([]string{}, docs...), docChunks...)); err != nil {
		return false, err
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
	// ENG-2122: the derivation journal's rows carry the document's source metadata
	// (title, author); a pending fact or triplet job of an erased document has nothing
	// left to derive.
	if err := run.exec(ctx, eraseDerivationJobs, `DELETE FROM `+TableDerivationJobs+` WHERE document_id = ANY($1)`, docs); err != nil {
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
// cascade), observations by evidence_id, links by evidence_id or supporting evidence,
// the outbox rows, then the evidence.
// entities.first_seen_evidence and evidence.revises_id become NULL by FK action (0051).
func (s *PgErasureStore) eraseEvidence(ctx context.Context, run *erasureRun, evidenceIDs []string) error {
	// ENG-1858: the entities the erased observations describe, read before they go, so
	// their ADR-0149 object rows are recomputed from what survives (an object row must
	// never keep a property value whose only source was erased — the query plane serves
	// those rows to principals).
	obsEntities := map[string][]string{}
	obsRows, err := run.tx.Query(ctx, `SELECT DISTINCT namespace_id, entity_id FROM observations
		WHERE evidence_id = ANY($1) ORDER BY 1, 2`, evidenceIDs)
	if err != nil {
		return fmt.Errorf("erase: observation entities: %w", err)
	}
	for obsRows.Next() {
		var ns, e string
		if err := obsRows.Scan(&ns, &e); err != nil {
			obsRows.Close()
			return err
		}
		obsEntities[ns] = append(obsEntities[ns], e)
	}
	obsRows.Close()
	if err := obsRows.Err(); err != nil {
		return err
	}
	if err := s.eraseEvidenceRows(ctx, run, evidenceIDs); err != nil {
		return err
	}
	nss := make([]string, 0, len(obsEntities))
	for ns := range obsEntities {
		nss = append(nss, ns)
	}
	sort.Strings(nss)
	for _, ns := range nss {
		if err := objects.ApplyStateTx(ctx, run.tx, s.l.k.kinds, ns, obsEntities[ns]...); err != nil {
			return fmt.Errorf("erase: object rows (%s): %w", ns, err)
		}
	}
	return nil
}

func (s *PgErasureStore) eraseEvidenceRows(ctx context.Context, run *erasureRun, evidenceIDs []string) error {
	// S7: plugin rows referencing the evidence go first, in the same transaction, so
	// none of them fails the evidence FK below.
	for _, p := range s.participants {
		counts, err := p.EraseEvidence(ctx, erasureTx{tx: run.tx}, evidenceIDs)
		if err != nil {
			return fmt.Errorf("erase: participant %s: %w", p.Name(), err)
		}
		for store, n := range counts {
			if n > 0 {
				run.counts[store] += n
			}
		}
	}
	if _, err := s.eraseItemsWhere(ctx, run, `ki.evidence_id = ANY($1)`, []any{evidenceIDs},
		domain.KnowledgeErasure{EvidenceIDs: evidenceIDs}); err != nil {
		return err
	}
	for _, st := range []struct{ store, sql string }{
		{eraseEvents, `DELETE FROM events WHERE evidence_id = ANY($1)`},
		{eraseObservations, `DELETE FROM observations WHERE evidence_id = ANY($1)`},
		// A link goes when its cited evidence OR any evidence it was derived from
		// is erased (ENG-2032): an identity producer's proposal, and the
		// confirmation inheriting it, record their supporting rows, so "E-17 same_as
		// P" does not outlive a forgotten row it was built from. Served by
		// idx_links_supporting_evidence (0056).
		{eraseLinks, `DELETE FROM links WHERE evidence_id = ANY($1) OR supporting_evidence && $1::text[]`},
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
	if err := s.eraseQuoters(ctx, run, found); err != nil {
		return false, err
	}
	for _, e := range found {
		run.target(domain.ErasureTargetExperience, e, "", "")
	}
	// experience_derivations cascade from experiences.
	return true, run.exec(ctx, eraseExperiences, `DELETE FROM `+TableExperiences+` WHERE id = ANY($1)`, found)
}

// eraseChunks erases single chunks and their derivatives: fact items keyed into them
// (source_chunk value), KG triplets and PageRank (deleted explicitly so they are counted;
// the 0051 FK would cascade them), the chunks, then structure/Hebbian edges.
//
// Edges go AFTER the chunk rows (ENG-1951): document_edges has no FK, and the async
// Hebbian write (PgVectorAdapter.SaveChunkEdges) fences on a FOR KEY SHARE lock of its
// endpoint chunk rows. Deleting the chunks first makes this erase wait for a writer that
// locked first, and the edge DELETE that follows then sees what it wrote; deleting the
// edges first would leave a window in which that writer re-creates an edge to a chunk
// this erase is about to delete.
func (s *PgErasureStore) eraseChunks(ctx context.Context, run *erasureRun, chunkIDs []string) error {
	if _, err := s.eraseItemsWhere(ctx, run,
		`EXISTS (SELECT 1 FROM statement_values sv WHERE sv.item_id = ki.id AND sv.predicate = $2 AND sv.value_text = ANY($1))`,
		[]any{chunkIDs, domain.FactPredSourceChunk},
		domain.KnowledgeErasure{ValuePredicate: domain.FactPredSourceChunk, ValueTexts: chunkIDs}); err != nil {
		return err
	}
	if err := s.eraseQuoters(ctx, run, chunkIDs); err != nil {
		return err
	}
	for _, st := range []struct{ store, sql string }{
		{cascadeTriplets, `DELETE FROM chunk_triplets WHERE chunk_id = ANY($1)`},
		{cascadePageRank, `DELETE FROM chunk_pagerank WHERE chunk_id = ANY($1)`},
		{cascadeChunks, `DELETE FROM ` + TableChunks + ` WHERE id = ANY($1)`},
		{cascadeEdges, `DELETE FROM document_edges WHERE source_id = ANY($1) OR target_id = ANY($1)`},
	} {
		if err := run.exec(ctx, st.store, st.sql, chunkIDs); err != nil {
			return err
		}
	}
	return nil
}

// eraseQuoters erases what quotes the memory records refs names, wherever an erase
// reaches records: the run-ledger entries built from them (eraseLedger) and the chat
// replies that rest on them (redactReplies).
func (s *PgErasureStore) eraseQuoters(ctx context.Context, run *erasureRun, refs []string) error {
	if err := s.eraseLedger(ctx, run, refs); err != nil {
		return err
	}
	return s.redactReplies(ctx, run, refs)
}

// redactReplies replaces the text of every agent reply whose recorded sources
// (conversation_messages.source_refs, written by the chat lane with the reply from
// its turn's ledger entries) meet refs with domain.RedactedReplyText — owner decision
// 2026-11-18, option B (ENG-2109): a reply that quoted a forgotten document is
// derived content, so the erase reaches it, but the conversation must stay readable,
// so the row, its role, seq and created_at stay and only the text goes. One
// content-free tombstone per reply (target kind conversation_message) puts its id on
// the receipt (ErasureReceipt.RedactedMessages). The person's own messages and
// interruption markers are never touched (role = 'agent' only), and a reply an
// earlier erase redacted is not counted again. It cannot come back: the text is
// overwritten in this transaction, and a later turn cannot recall the erased record.
//
// Reach (ENG-2155): a reply records what its turn recalled AND what it was given — the
// transcript's replies, the rolling summary, the turn index — so a later reply that
// repeats an earlier one's value is redacted with it, and a message the consent bridge
// posts during a turn records that turn's sources. Not reached (ADR-0137 inventory):
// a message emitted with no turn running records none; a reply stored before
// migration 0060 whose turn's ledger entries had expired by 0061's backfill.
func (s *PgErasureStore) redactReplies(ctx context.Context, run *erasureRun, refs []string) error {
	if len(refs) == 0 {
		return nil
	}
	ids, err := collectIDs(ctx, run.tx, `UPDATE conversation_messages
		   SET content = $2, redacted_at = now()
		 WHERE role = $3 AND redacted_at IS NULL AND source_refs ?| $1::text[]
		RETURNING id`, refs, domain.RedactedReplyText, string(domain.MessageRoleAgent))
	if err != nil {
		return fmt.Errorf("erase %s: %w", eraseConversationMessages, err)
	}
	if len(ids) > 0 {
		run.counts[eraseConversationMessages] += len(ids)
	}
	for _, id := range ids {
		run.target(domain.ErasureTargetConversationMessage, id, "", "")
	}
	return nil
}

// eraseLedger deletes the run-ledger entries whose source_refs meet refs (memory
// record ids: chunks, documents, experiences) — ADR-0157 D5.6, the ADR-0137
// cascade's ledger step. source_refs is transitive (the writer carries a source's
// refs into every entry derived from it: a step output, an artifact built from a
// recall), so one indexed match (GIN, ?|) reaches the whole derivation chain. Each
// erased entry's vault body is queued (vault_delete) for the worker, which deletes
// it only when no surviving entry names the same bytes. A kernel without the
// ledger table skips the step.
//
// Not reached (recorded in the ADR-0137 inventory): ledger entries that quote
// memory without naming it — a tool result of an external tool that returned
// memory text, a model's step output that paraphrases a recall it did not
// inherit — expire with execution.ledger.retention_hours.
func (s *PgErasureStore) eraseLedger(ctx context.Context, run *erasureRun, refs []string) error {
	if len(refs) == 0 {
		return nil
	}
	if !run.ledgerChecked {
		ok, err := tableExists(ctx, run.tx, "ledger_entries")
		if err != nil {
			return err
		}
		run.ledgerChecked, run.hasLedger = true, ok
	}
	if !run.hasLedger {
		return nil
	}
	rows, err := run.tx.Query(ctx,
		`DELETE FROM ledger_entries WHERE source_refs ?| $1::text[] RETURNING body_hash, tool_name`, refs)
	if err != nil {
		return fmt.Errorf("erase %s: %w", eraseLedgerEntries, err)
	}
	defer rows.Close()
	for rows.Next() {
		var hash, tool string
		if err := rows.Scan(&hash, &tool); err != nil {
			return fmt.Errorf("erase %s: %w", eraseLedgerEntries, err)
		}
		run.counts[eraseLedgerEntries]++
		// ENG-2155: a rolling summary that condensed a quoting reply goes with it; the
		// chat lane folds the redacted transcript again on the next turn.
		if tool == domain.LedgerToolConversationSummary {
			run.counts[eraseConversationSummaries]++
		}
		if hash != "" {
			run.bodies[hash] = true
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("erase %s: %w", eraseLedgerEntries, err)
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
	for _, l := range [][]string{sel.DocumentIDs, sel.ThreadIDs, sel.ChunkIDs, sel.ExperienceIDs, sel.EntityRefs, sel.MemoryNoteIDs, sel.MemoryNoteOwners} {
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

// ListReceipts implements domain.MemoryEraser: receipts newest first (erased_at, then
// request id), filtered by principal, paged by the Before cursor.
func (s *PgErasureStore) ListReceipts(ctx context.Context, f domain.ErasureReceiptFilter) ([]domain.ErasureReceipt, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	ids, err := collectIDs(ctx, s.l.k.pool, `SELECT e.request_id FROM memory_erasures e
		WHERE ($1 = '' OR e.principal = $1)
		  AND ($2 = '' OR (e.erased_at, e.request_id) < (
		        SELECT c.erased_at, c.request_id FROM memory_erasures c WHERE c.request_id = $2))
		ORDER BY e.erased_at DESC, e.request_id DESC
		LIMIT $3`, f.Principal, f.Before, limit)
	if err != nil {
		return nil, mapError("ListReceipts", err)
	}
	out := make([]domain.ErasureReceipt, 0, len(ids))
	for _, id := range ids {
		rec, found, err := receiptFrom(ctx, s.l.k.pool, id)
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, rec)
		}
	}
	return out, nil
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
	// Steps that ended because their records live in an external service the kernel
	// cannot delete from (ADR-0137 D7): processed, with the terminal outcome class.
	notErasable, err := collectIDs(ctx, q, `SELECT DISTINCT step FROM memory_erasure_outbox
		WHERE request_id = $1 AND processed_at IS NOT NULL AND last_error = $2
		ORDER BY step`, requestID, domain.ErasureOutcomeNotErasableExternal)
	if err != nil {
		return domain.ErasureReceipt{}, false, mapError("Receipt", err)
	}
	if len(notErasable) > 0 {
		rec.NotErasableExternal = notErasable
	}
	// ENG-2109: the agent replies the erase redacted, from their tombstones.
	redacted, err := collectIDs(ctx, q, `SELECT target_id FROM memory_erasure_targets
		WHERE request_id = $1 AND target_kind = $2 ORDER BY target_id`,
		requestID, domain.ErasureTargetConversationMessage)
	if err != nil {
		return domain.ErasureReceipt{}, false, mapError("Receipt", err)
	}
	if len(redacted) > 0 {
		rec.RedactedMessages = redacted
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
