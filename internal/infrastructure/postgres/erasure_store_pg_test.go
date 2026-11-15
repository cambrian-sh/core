package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0137 S2 (ENG-411 stage A) against a real, migrated Postgres (PG_TEST_DSN, migrated
// through 0051). Reuses the ADR-0138 lifecycle fixtures (namespace "lctest", ids
// "lctest-"); erasure rows are keyed by principals "lctest-…" and removed before and after.

const erasureSecret = "SECRET-BODY-erasure-test"

func newErasureStore(t *testing.T) (*PgVectorAdapter, *PgKnowledgeStore, *PgErasureStore, context.Context) {
	t.Helper()
	a, ks, lc, ctx := newLifecycleStores(t)
	if countRows(t, a, ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name = 'memory_erasures'`) == 0 {
		t.Skip("memory_erasures missing (run `orchestrator migrate up` first)")
	}
	clean := func() {
		c := context.Background()
		for _, q := range []string{
			`DROP TABLE IF EXISTS lctest_premium_ref`,
			`DELETE FROM memory_erasures WHERE principal LIKE 'lctest-%'`,
			`DELETE FROM events WHERE id LIKE 'lctest-%'`,
			`DELETE FROM evidence_outbox WHERE evidence_id LIKE 'lctest-%'`,
			`DELETE FROM evidence WHERE id LIKE 'lctest-%'`,
			`DELETE FROM documents_legacy WHERE id LIKE 'lctest-%'`,
			`DELETE FROM chunks WHERE id LIKE 'source_doc:lctest-%' OR experience_id LIKE 'lctest-%'`,
			`DELETE FROM experiences WHERE id LIKE 'lctest-%'`,
		} {
			_, _ = a.pool.Exec(c, q)
		}
	}
	clean()
	t.Cleanup(clean) // runs before the lifecycle fixture's own cleanup (LIFO)
	return a, ks, NewPgErasureStore(lc), ctx
}

func execSQL(t *testing.T, a *PgVectorAdapter, ctx context.Context, sql string, args ...any) {
	t.Helper()
	if _, err := a.pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// seedErasureEvidence gives document id an evidence row (keyed by the document id, as
// captureEvidence does), an outbox item, an event citing it, a documents_legacy copy and
// a source-doc entity stub whose body lives in the CAS.
func seedErasureEvidence(t *testing.T, a *PgVectorAdapter, ctx context.Context, id string) {
	t.Helper()
	ev := id + "-ev"
	execSQL(t, a, ctx, `INSERT INTO evidence (id, source_id, source_key, source_revision, content_hash)
		VALUES ($1, 'lctest-src', $2, '1', $3)`, ev, id, "cid-ev-"+id)
	execSQL(t, a, ctx, `INSERT INTO evidence_outbox (evidence_id) VALUES ($1)`, ev)
	execSQL(t, a, ctx, `INSERT INTO events (id, event_type, occurred_at, evidence_id) VALUES ($1, 'lctest', now(), $2)`, id+"-event", ev)
	execSQL(t, a, ctx, `INSERT INTO documents_legacy (id, text, metadata) VALUES ($1, $2, jsonb_build_object('document_id', $3::text))`,
		id+"-legacy-1", erasureSecret, id)
	execSQL(t, a, ctx, `INSERT INTO chunks (id, document_id, text, metadata) VALUES ($1, $2, $3,
		jsonb_build_object('document_id', $2::text, 'content_cid', $4::text))`,
		"source_doc:"+id, id, erasureSecret, "cid-body-"+id)
}

func erasureReq(principal string, sel domain.ErasureSelector) domain.ErasureRequest {
	return domain.ErasureRequest{Principal: principal, PrincipalKind: domain.ErasureByOperator,
		Reason: domain.ErasureReasonCompliance, Selector: sel}
}

// The load-bearing test: one request removes the document and every derivative S2 owns,
// with exact per-store counts; an unrelated document is untouched; the record left behind
// is content-free; a retry returns the same receipt and deletes nothing.
func TestPg_Erase_DocumentAndEveryDerivative(t *testing.T) {
	a, ks, er, ctx := newErasureStore(t)
	seedLifecycleDoc(t, a, ks, ctx, "lctest-doc", "agent:a", erasureSecret, "dg-1", 2)
	seedLifecycleDoc(t, a, ks, ctx, "lctest-other", "agent:b", "other text", "dg-2", 2)
	seedErasureEvidence(t, a, ctx, "lctest-doc")
	otherBefore := derivativeCounts(t, a, ctx, "lctest-other")

	req := erasureReq("lctest-op", domain.ErasureSelector{DocumentIDs: []string{"lctest-doc"}})
	rec, err := er.Erase(ctx, req)
	if err != nil {
		t.Fatalf("Erase: %v", err)
	}
	want := map[string]int{
		"documents": 1, "chunks": 3, // two chunks + the source-doc stub
		"chunk_triplets": 1, "chunk_pagerank": 1, "document_sections": 1,
		"document_edges":  2, // PART_OF + the Hebbian edge from lctest-other
		"knowledge_items": 1, "knowledge_extractions": 1, "knowledge_extraction_windows": 1,
		"evidence": 1, "evidence_outbox": 1, "events": 1, "documents_legacy": 1,
	}
	for k, v := range want {
		if rec.Counts[k] != v {
			t.Errorf("count %s = %d, want %d (all: %v)", k, rec.Counts[k], v, rec.Counts)
		}
	}
	if rec.RequestID != req.ID() || rec.Replayed || rec.Status != domain.ErasureStatusPendingAsync ||
		rec.PendingSteps != 2 || rec.Targets != 2 || rec.ErasedAt.IsZero() {
		t.Fatalf("receipt = %+v", rec)
	}
	for k, v := range derivativeCounts(t, a, ctx, "lctest-doc") {
		if v != 0 {
			t.Errorf("after erase: %s = %d", k, v)
		}
	}
	for _, q := range []string{
		`SELECT count(*) FROM evidence WHERE source_key = 'lctest-doc'`,
		`SELECT count(*) FROM events WHERE id = 'lctest-doc-event'`,
		`SELECT count(*) FROM documents_legacy WHERE id LIKE 'lctest-doc%'`,
		`SELECT count(*) FROM chunks WHERE text = '` + erasureSecret + `'`,
	} {
		if n := countRows(t, a, ctx, q); n != 0 {
			t.Errorf("%s = %d after erase", q, n)
		}
	}
	// The unrelated document keeps everything — except the one Hebbian edge from its chunk
	// INTO the erased chunk, which must go with it (an edge touching erased content).
	otherBefore["edges"]--
	for k, v := range derivativeCounts(t, a, ctx, "lctest-other") {
		if v != otherBefore[k] {
			t.Errorf("unrelated document lost %s: want %d, got %d", k, otherBefore[k], v)
		}
	}
	if n := countRows(t, a, ctx, `SELECT count(*) FROM document_edges WHERE target_id = 'lctest-doc-chunk-1'`); n != 0 {
		t.Errorf("edge into the erased chunk survived (%d)", n)
	}

	// Content-free: no column of the receipt, tombstones or outbox holds the body.
	for _, q := range []string{
		`SELECT coalesce(string_agg(row_to_json(e)::text, ''), '') FROM memory_erasures e WHERE request_id = $1`,
		`SELECT coalesce(string_agg(row_to_json(t)::text, ''), '') FROM memory_erasure_targets t WHERE request_id = $1`,
		`SELECT coalesce(string_agg(row_to_json(o)::text, ''), '') FROM memory_erasure_outbox o WHERE request_id = $1`,
	} {
		var s string
		if err := a.pool.QueryRow(ctx, q, rec.RequestID).Scan(&s); err != nil {
			t.Fatal(err)
		}
		if s == "" || strings.Contains(s, "SECRET") || strings.Contains(s, "boat") {
			t.Errorf("record is empty or holds content: %s", s)
		}
	}
	if n := countRows(t, a, ctx, `SELECT count(*) FROM memory_erasure_targets
		WHERE request_id = $1 AND target_kind = 'document' AND target_id = 'lctest-doc' AND content_digest = 'dg-1'`, rec.RequestID); n != 1 {
		t.Error("document tombstone with its digest missing")
	}

	// Idempotent replay: the same receipt, nothing deleted, one record.
	again, err := er.Erase(ctx, req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !again.Replayed || again.RequestID != rec.RequestID || !again.ErasedAt.Equal(rec.ErasedAt) ||
		again.Targets != rec.Targets || len(again.Counts) != len(rec.Counts) {
		t.Fatalf("replay receipt %+v differs from %+v", again, rec)
	}
	for k, v := range rec.Counts {
		if again.Counts[k] != v {
			t.Errorf("replay count %s = %d, want %d", k, again.Counts[k], v)
		}
	}
	if n := countRows(t, a, ctx, `SELECT count(*) FROM memory_erasures WHERE request_id = $1`, rec.RequestID); n != 1 {
		t.Fatalf("%d receipts for one request", n)
	}
	if got, found, err := er.Receipt(ctx, rec.RequestID); err != nil || !found || got.Status != rec.Status {
		t.Fatalf("Receipt = %+v %v %v", got, found, err)
	}

	// A different request for the same, already-erased ids: nothing to erase, with a
	// pointer to the request that erased them.
	_, err = er.Erase(ctx, erasureReq("lctest-op2", domain.ErasureSelector{DocumentIDs: []string{"lctest-doc"}}))
	var nte *domain.NothingToEraseError
	if !errors.Is(err, domain.ErrNothingToErase) || !errors.As(err, &nte) || nte.EarlierRequestID != rec.RequestID {
		t.Fatalf("want NothingToErase pointing at %s, got %v", rec.RequestID, err)
	}
}

// A failure in the middle of the transaction (evidence still referenced by a row the
// eraser does not own — the premium ingress-studio shape until S7) rolls everything back:
// the document, its chunks and derivatives are all still there and no receipt exists.
func TestPg_Erase_MidTransactionFailureRollsBack(t *testing.T) {
	a, ks, er, ctx := newErasureStore(t)
	seedLifecycleDoc(t, a, ks, ctx, "lctest-rb", "agent:a", erasureSecret, "dg-rb", 2)
	seedErasureEvidence(t, a, ctx, "lctest-rb")
	execSQL(t, a, ctx, `CREATE TABLE lctest_premium_ref (evidence_id TEXT REFERENCES evidence(id))`)
	execSQL(t, a, ctx, `INSERT INTO lctest_premium_ref VALUES ('lctest-rb-ev')`)
	before := derivativeCounts(t, a, ctx, "lctest-rb")

	_, err := er.Erase(ctx, erasureReq("lctest-op", domain.ErasureSelector{DocumentIDs: []string{"lctest-rb"}}))
	if err == nil {
		t.Fatal("erase succeeded although evidence is still referenced")
	}
	for k, v := range derivativeCounts(t, a, ctx, "lctest-rb") {
		if v != before[k] {
			t.Errorf("rolled-back erase changed %s: %d -> %d", k, before[k], v)
		}
	}
	if n := countRows(t, a, ctx, `SELECT count(*) FROM evidence WHERE id = 'lctest-rb-ev'`) +
		countRows(t, a, ctx, `SELECT count(*) FROM documents_legacy WHERE id = 'lctest-rb-legacy-1'`); n != 2 {
		t.Errorf("evidence / legacy rows lost in a rolled-back erase (%d of 2)", n)
	}
	if n := countRows(t, a, ctx, `SELECT count(*) FROM memory_erasures WHERE principal = 'lctest-op'`); n != 0 {
		t.Fatalf("a failed erase left %d receipts", n)
	}
}

// Chunk, thread and experience selectors (D2), and the empty-match answer.
func TestPg_Erase_ChunkThreadExperienceSelectors(t *testing.T) {
	a, ks, er, ctx := newErasureStore(t)
	seedLifecycleDoc(t, a, ks, ctx, "lctest-multi", "agent:a", "multi", "dg-m", 2)
	seedLifecycleDoc(t, a, ks, ctx, "lctest-solo", "agent:a", "solo", "dg-s", 1)

	// One chunk of a two-chunk document: that chunk and its derivatives only.
	rec, err := er.Erase(ctx, erasureReq("lctest-op", domain.ErasureSelector{ChunkIDs: []string{"lctest-multi-chunk-1"}}))
	if err != nil {
		t.Fatalf("chunk erase: %v", err)
	}
	if rec.Counts["chunks"] != 1 || rec.Counts["chunk_triplets"] != 1 || rec.Counts["knowledge_items"] != 1 || rec.Counts["documents"] != 0 {
		t.Errorf("single-chunk counts = %v", rec.Counts)
	}
	if countRows(t, a, ctx, `SELECT count(*) FROM chunks WHERE id = 'lctest-multi-chunk-2'`) != 1 ||
		countRows(t, a, ctx, `SELECT count(*) FROM documents WHERE id = 'lctest-multi'`) != 1 {
		t.Error("erasing one chunk removed its sibling or its document")
	}

	// The only chunk of a document: the document goes (D2).
	rec, err = er.Erase(ctx, erasureReq("lctest-op", domain.ErasureSelector{ChunkIDs: []string{"lctest-solo-chunk-1"}}))
	if err != nil {
		t.Fatalf("solo chunk erase: %v", err)
	}
	if rec.Counts["documents"] != 1 || countRows(t, a, ctx, `SELECT count(*) FROM documents WHERE id = 'lctest-solo'`) != 0 {
		t.Errorf("a document's only chunk did not erase the document: %v", rec.Counts)
	}

	// Thread: documents whose chunks arrived on it.
	execSQL(t, a, ctx, `UPDATE chunks SET metadata = metadata || '{"ingest_thread_id":"lctest-thread"}' WHERE document_id = 'lctest-multi'`)
	rec, err = er.Erase(ctx, erasureReq("lctest-op", domain.ErasureSelector{ThreadIDs: []string{"lctest-thread"}}))
	if err != nil {
		t.Fatalf("thread erase: %v", err)
	}
	if rec.Counts["documents"] != 1 || countRows(t, a, ctx, `SELECT count(*) FROM chunks WHERE document_id = 'lctest-multi'`) != 0 {
		t.Errorf("thread erase: %v", rec.Counts)
	}

	// Experience: the parent row and its chunks (with their KG rows) go.
	execSQL(t, a, ctx, `INSERT INTO experiences (id, outcome) VALUES ('lctest-exp', 'success')`)
	execSQL(t, a, ctx, `INSERT INTO chunks (id, experience_id, text) VALUES ('lctest-exp-c1', 'lctest-exp', $1)`, erasureSecret)
	execSQL(t, a, ctx, `INSERT INTO chunk_triplets (chunk_id, h, r, t) VALUES ('lctest-exp-c1', 'x', 'r', 'y')`)
	rec, err = er.Erase(ctx, erasureReq("lctest-op", domain.ErasureSelector{ExperienceIDs: []string{"lctest-exp"}}))
	if err != nil {
		t.Fatalf("experience erase: %v", err)
	}
	if rec.Counts["experiences"] != 1 || rec.Counts["chunks"] != 1 || rec.Counts["chunk_triplets"] != 1 || rec.Status != domain.ErasureStatusComplete {
		t.Errorf("experience erase: %+v", rec)
	}

	// Nothing stored under the ids, never erased before: plain ErrNothingToErase.
	_, err = er.Erase(ctx, erasureReq("lctest-op", domain.ErasureSelector{DocumentIDs: []string{"lctest-never"}}))
	var nte *domain.NothingToEraseError
	if !errors.As(err, &nte) || nte.EarlierRequestID != "" {
		t.Fatalf("want bare NothingToErase, got %v", err)
	}
	// An invalid request never reaches the database.
	if _, err := er.Erase(ctx, domain.ErasureRequest{Principal: "lctest-op"}); !errors.Is(err, domain.ErrInvalidErasureRequest) {
		t.Fatalf("invalid request: %v", err)
	}
}

// Entity selector: substrate items about the entity go through EraseItemsTx; chunks that
// merely mention it are not erased.
func TestPg_Erase_EntitySelectorErasesSubstrateItemsOnly(t *testing.T) {
	a, ks, er, ctx := newErasureStore(t)
	seedLifecycleDoc(t, a, ks, ctx, "lctest-ent", "agent:a", "alice owns a boat", "dg-e", 1)
	var entity string
	if err := a.pool.QueryRow(ctx, `SELECT entity_id FROM knowledge_items WHERE namespace_id = $1 LIMIT 1`, lcNS).Scan(&entity); err != nil {
		t.Fatal(err)
	}
	rec, err := er.Erase(ctx, erasureReq("lctest-op", domain.ErasureSelector{EntityRefs: []string{entity}}))
	if err != nil {
		t.Fatalf("entity erase: %v", err)
	}
	if rec.Counts["knowledge_items"] < 1 || rec.Counts["chunks"] != 0 {
		t.Errorf("entity erase counts = %v", rec.Counts)
	}
	if countRows(t, a, ctx, `SELECT count(*) FROM knowledge_items WHERE namespace_id = $1 AND entity_id = $2`, lcNS, entity) != 0 {
		t.Error("items about the entity survived")
	}
	if countRows(t, a, ctx, `SELECT count(*) FROM chunks WHERE document_id = 'lctest-ent'`) != 1 {
		t.Error("an entity erase must not delete chunks that merely mention it")
	}
}
