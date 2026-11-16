package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/memory/erasureworker"
)

// ADR-0137 S3–S5 (ENG-411 stage B) end to end against a real, migrated Postgres
// (PG_TEST_DSN through 0051): the enforcing eraser over the real store, the
// no-resurrection fence on every content write path, and the worker draining the outbox.
// Fixtures as stage A (ids "lctest-", principals "lctest-…").

func agentEraseCtx(id string) context.Context {
	return domain.WithScope(domain.WithPrincipal(context.Background(), domain.AgentPrincipal(id)), domain.ScopeSystem)
}

func operatorEraseCtx() context.Context {
	ctx := domain.WithPrincipal(context.Background(), domain.UserPrincipal("lctest-op"))
	return domain.WithPrincipalRoles(domain.WithScope(ctx, domain.ScopeSystem), []string{"operator"})
}

func forget(sel domain.ErasureSelector) domain.ErasureRequest {
	return domain.ErasureRequest{Reason: domain.ErasureReasonUserRequest, Selector: sel,
		Principal: "ignored", PrincipalKind: domain.ErasureByKernel}
}

// Gate TestForget_ScopeDenied (Postgres half): agent A cannot erase B's document, can
// erase its own; an operator can erase B's; an unset scope fails closed; a document
// outside A's predicate is nothing-to-erase, not denied.
func TestPg_EnforcingEraser_WriterOnlyOperatorAnyUnsetScopeFailsClosed(t *testing.T) {
	a, ks, er, ctx := newErasureStore(t)
	seedLifecycleDoc(t, a, ks, ctx, "lctest-a-doc", "lctest-a", "a text", "dg-a", 1)
	seedLifecycleDoc(t, a, ks, ctx, "lctest-b-doc", "lctest-b", "b text", "dg-b", 1)
	seedLifecycleDoc(t, a, ks, ctx, "lctest-hr-doc", "lctest-a", "hr text", "dg-hr", 1)
	execSQL(t, a, ctx, `UPDATE documents SET tags = '{hr}' WHERE id = 'lctest-hr-doc'`)
	e := authz.NewEnforcingEraser(er, domain.AllowAllAuthorizer{}, authz.EraserPolicy{AgentErase: true, MaxTargets: 100}, nil)
	docs := func(id string) int { return countRows(t, a, ctx, `SELECT count(*) FROM documents WHERE id = $1`, id) }

	_, err := e.Erase(agentEraseCtx("lctest-a"), forget(domain.ErasureSelector{DocumentIDs: []string{"lctest-b-doc"}}))
	if !errors.Is(err, authz.ErrEraseDenied) {
		t.Fatalf("A erasing B's document: want ErrEraseDenied, got %v", err)
	}
	// The chunk selector widens to the document; the writer of record is the document's.
	_, err = e.Erase(agentEraseCtx("lctest-a"), forget(domain.ErasureSelector{ChunkIDs: []string{"lctest-b-doc-chunk-1"}}))
	if !errors.Is(err, authz.ErrEraseDenied) {
		t.Fatalf("A erasing B's chunk: want ErrEraseDenied, got %v", err)
	}
	if docs("lctest-b-doc") != 1 || countRows(t, a, ctx, `SELECT count(*) FROM chunks WHERE id = 'lctest-b-doc-chunk-1'`) != 1 {
		t.Fatal("a denied erase must leave B's document and chunk")
	}

	noScope := domain.WithPrincipal(context.Background(), domain.AgentPrincipal("lctest-a"))
	if _, err := e.Erase(noScope, forget(domain.ErasureSelector{DocumentIDs: []string{"lctest-a-doc"}})); !errors.Is(err, authz.ErrScopeMissing) {
		t.Fatalf("unset scope: want ErrScopeMissing, got %v", err)
	}

	team := domain.WithScope(domain.WithPrincipal(context.Background(), domain.AgentPrincipal("lctest-a")),
		&domain.TagPredicate{ForbiddenTags: []string{"hr"}})
	if _, err := e.Erase(team, forget(domain.ErasureSelector{DocumentIDs: []string{"lctest-hr-doc"}})); !errors.Is(err, domain.ErrNothingToErase) {
		t.Fatalf("a hidden document: want ErrNothingToErase, got %v", err)
	}
	if docs("lctest-hr-doc") != 1 {
		t.Fatal("a hidden document must not be erased")
	}

	rec, err := e.Erase(agentEraseCtx("lctest-a"), forget(domain.ErasureSelector{DocumentIDs: []string{"lctest-a-doc"}}))
	if err != nil {
		t.Fatalf("A erasing its own document: %v", err)
	}
	if rec.Principal != "lctest-a" || rec.PrincipalKind != domain.ErasureByAgent || docs("lctest-a-doc") != 0 {
		t.Fatalf("receipt = %+v, document rows = %d", rec, docs("lctest-a-doc"))
	}
	if _, found, _ := e.Receipt(agentEraseCtx("lctest-b"), rec.RequestID); found {
		t.Fatal("B must not see A's receipt")
	}

	if _, err := e.Erase(operatorEraseCtx(), forget(domain.ErasureSelector{DocumentIDs: []string{"lctest-b-doc"}})); err != nil {
		t.Fatalf("operator erasing B's document: %v", err)
	}
	if docs("lctest-b-doc") != 0 {
		t.Fatal("the operator's erase did not remove B's document")
	}
}

// Gate TestForget_NoResurrection / TestForget_ReingestIsNewAct (store half): writes that
// carry an ingest origin from BEFORE the erase — a queued ingest, a replay, an in-flight
// triplet batch or fact job — are refused on every content path and write nothing; a
// genuinely new ingest after the erase is allowed.
func TestPg_ErasureFence_InFlightWritesForAnErasedDocumentWriteNothing(t *testing.T) {
	a, ks, er, ctx := newErasureStore(t)
	seedLifecycleDoc(t, a, ks, ctx, "lctest-f", "lctest-a", erasureSecret, "dg-f", 2)
	origin := time.Now().Add(-time.Minute) // the in-flight work began before the erase
	if _, err := er.Erase(ctx, erasureReq("lctest-op", domain.ErasureSelector{DocumentIDs: []string{"lctest-f"}})); err != nil {
		t.Fatalf("Erase: %v", err)
	}
	stale := domain.WithIngestOrigin(ctx, origin)

	if _, err := a.SaveDocument(stale, domain.SourceDocument{ID: "lctest-f", Text: erasureSecret, WrittenBy: "lctest-a", ContentDigest: "dg-f"}); !errors.Is(err, domain.ErrSourceErased) {
		t.Fatalf("replayed document row: want ErrSourceErased, got %v", err)
	}
	// The same content under another id (a re-keyed replay) is fenced by its digest.
	if _, err := a.SaveDocument(stale, domain.SourceDocument{ID: "lctest-f2", Text: erasureSecret, WrittenBy: "lctest-a", ContentDigest: "dg-f"}); !errors.Is(err, domain.ErrSourceErased) {
		t.Fatalf("replayed content under a new id: want ErrSourceErased, got %v", err)
	}
	chunk := &domain.Document{ID: "lctest-f-chunk-1", Text: erasureSecret, DocumentType: domain.DocTypeMnemonicFact,
		Embedding: domain.Embedding{Vector: unitVec(1), Size: eventTestDim, Model: "lc-model"},
		Metadata:  map[string]any{"document_id": "lctest-f"}, CreatedAt: time.Now().UTC()}
	if err := a.SaveBatch(stale, []*domain.Document{chunk}); !errors.Is(err, domain.ErrSourceErased) {
		t.Fatalf("queued chunk batch: want ErrSourceErased, got %v", err)
	}
	if err := a.Save(stale, chunk); !errors.Is(err, domain.ErrSourceErased) {
		t.Fatalf("single chunk save: want ErrSourceErased, got %v", err)
	}
	// An in-flight triplet batch for the erased chunk (the 0051 FK and the fence).
	if err := a.SaveChunkTriplets(ctx, "lctest-f-chunk-1", []domain.ChunkTriplet{{H: "a", R: "r", T: "b", Weight: 1}}); !errors.Is(err, domain.ErrSourceErased) {
		t.Fatalf("triplet batch for an erased chunk: want ErrSourceErased, got %v", err)
	}
	// An in-flight fact job of the erased document.
	f := domain.Fact{Subject: "alice", Attribute: "owns", Value: "boat", SubjectKey: "alice", AttributeKey: "owns",
		ValueNorm: domain.NormalizeFactText("boat"), AssertedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Actor: "lctest-a", SourceChunkID: "lctest-f-chunk-1", SourceDocumentID: "lctest-f", Quote: "boat"}
	if _, _, err := ks.PutItem(stale, f.ToItem(lcNS)); !errors.Is(err, domain.ErrSourceErased) {
		t.Fatalf("fact job of an erased document: want ErrSourceErased, got %v", err)
	}
	for _, q := range []string{
		`SELECT count(*) FROM documents WHERE id IN ('lctest-f', 'lctest-f2')`,
		`SELECT count(*) FROM chunks WHERE id LIKE 'lctest-f-%' OR text = '` + erasureSecret + `'`,
		`SELECT count(*) FROM chunk_triplets WHERE chunk_id LIKE 'lctest-f-%'`,
		`SELECT count(*) FROM knowledge_items ki JOIN statement_values sv ON sv.item_id = ki.id
			WHERE ki.namespace_id = 'lctest' AND sv.predicate = 'source_document' AND sv.value_text = 'lctest-f'`,
	} {
		if n := countRows(t, a, ctx, q); n != 0 {
			t.Errorf("resurrected: %s = %d", q, n)
		}
	}

	// A new act after the erase (origin later than the tombstone) is a re-ingest (Q4).
	fresh := domain.WithIngestOrigin(ctx, time.Now().Add(time.Second))
	if _, err := a.SaveDocument(fresh, domain.SourceDocument{ID: "lctest-f", Text: "new text", WrittenBy: "lctest-a", ContentDigest: "dg-f"}); err != nil {
		t.Fatalf("a re-ingest after the erase must be allowed: %v", err)
	}
	if err := a.SaveBatch(fresh, []*domain.Document{chunk}); err != nil {
		t.Fatalf("its chunks too: %v", err)
	}
	// Kernel-authored writes carry no origin and are not fenced.
	if _, err := a.SaveDocument(ctx, domain.SourceDocument{ID: "lctest-kernel", Text: "k"}); err != nil {
		t.Fatalf("origin-less write: %v", err)
	}
}

// The fence serialises with the erase: a write that holds its document's shared lock
// makes the erase wait, so the erase deletes what the write committed — never the
// reverse order with the write landing after the erase.
func TestPg_ErasureFence_ConcurrentWriteCommitsBeforeTheEraseDeletesIt(t *testing.T) {
	a, ks, er, ctx := newErasureStore(t)
	seedLifecycleDoc(t, a, ks, ctx, "lctest-c", "lctest-a", "c text", "dg-c", 1)
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := erasureFenceTx(domain.WithIngestOrigin(ctx, time.Now()), tx, "lctest-c", ""); err != nil {
		t.Fatalf("fence: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO chunks (id, document_id, text, metadata) VALUES ('lctest-c-late', 'lctest-c', 'late', '{}')`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := er.Erase(ctx, erasureReq("lctest-op", domain.ErasureSelector{DocumentIDs: []string{"lctest-c"}}))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the erase must wait for the fenced write, returned %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if n := countRows(t, a, ctx, `SELECT count(*) FROM chunks WHERE document_id = 'lctest-c' OR id = 'lctest-c-late'`); n != 0 {
		t.Fatalf("the committed write survived the erase: %d rows", n)
	}
}

// fakeContent records CAS deletes (the bbolt store is unit-tested in internal/storage).
type fakeContent struct{ deleted []domain.CID }

func (f *fakeContent) DeleteContent(_ context.Context, cid domain.CID) error {
	f.deleted = append(f.deleted, cid)
	return nil
}

// S5: the erase stamps async_deadline; the worker deletes the unshared blob, keeps the
// one a surviving record still references, completes the request; observations citing
// the erased evidence go and their entities' object rows are recomputed (ENG-1858).
func TestPg_ErasureWorker_DrainsTheOutboxAndCompletesTheRequest(t *testing.T) {
	a, ks, er0, ctx := newErasureStore(t)
	er := NewPgErasureStore(er0.l, WithErasureAsyncDeadline(time.Hour))
	seedLifecycleDoc(t, a, ks, ctx, "lctest-w", "lctest-a", erasureSecret, "dg-w", 1)
	seedErasureEvidence(t, a, ctx, "lctest-w")
	execSQL(t, a, ctx, `INSERT INTO observations (namespace_id, entity_id, predicate, value_type, value_text, occurred_at, evidence_id, source_ref)
		VALUES ('lctest', 'lctest-ent', 'status', 'text', 'x', now(), 'lctest-w-ev', 'lctest-obs')`)
	t.Cleanup(func() {
		_, _ = a.pool.Exec(context.Background(), `DELETE FROM observations WHERE namespace_id = 'lctest'`)
	})
	// Another, surviving document's stub shares the source-doc body's CID.
	execSQL(t, a, ctx, `INSERT INTO chunks (id, text, metadata) VALUES ('source_doc:lctest-keep', 'k',
		jsonb_build_object('content_cid', 'cid-body-lctest-w'))`)

	rec, err := er.Erase(ctx, erasureReq("lctest-op", domain.ErasureSelector{DocumentIDs: []string{"lctest-w"}}))
	if err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if rec.Status != domain.ErasureStatusPendingAsync || rec.PendingSteps != 2 {
		t.Fatalf("receipt = %+v", rec)
	}
	if d := rec.AsyncDeadline.Sub(rec.ErasedAt); d < 59*time.Minute || d > 61*time.Minute {
		t.Fatalf("async_deadline - erased_at = %v, want 1h", d)
	}
	if n := countRows(t, a, ctx, `SELECT count(*) FROM observations WHERE evidence_id = 'lctest-w-ev'`); n != 0 {
		t.Fatalf("observations citing erased evidence survived: %d", n)
	}

	cas := &fakeContent{}
	st := erasureworker.New(er, cas, erasureworker.Config{Poll: time.Hour}, nil).RunOnce(ctx)
	if st.Done != 1 || st.Shared != 1 || st.Failed != 0 || st.Completed < 1 {
		t.Fatalf("worker stats = %+v", st)
	}
	if len(cas.deleted) != 1 || cas.deleted[0] != "cid-ev-lctest-w" {
		t.Fatalf("deleted = %v, want only the unshared evidence blob", cas.deleted)
	}
	got, found, err := er.Receipt(ctx, rec.RequestID)
	if err != nil || !found || got.Status != domain.ErasureStatusComplete || got.PendingSteps != 0 {
		t.Fatalf("receipt after drain = %+v found=%v err=%v", got, found, err)
	}
	// Exactly once: a second pass finds nothing.
	if st := erasureworker.New(er, cas, erasureworker.Config{Poll: time.Hour}, nil).RunOnce(ctx); st.Done+st.Shared+st.Failed != 0 {
		t.Fatalf("second pass did work: %+v", st)
	}
	// Retention: tombstones older than the cutoff go, the receipt stays.
	if n, err := er.PruneTombstones(ctx, time.Now().Add(time.Hour)); err != nil || n < 1 {
		t.Fatalf("PruneTombstones = %d, %v", n, err)
	}
	if _, found, _ := er.Receipt(ctx, rec.RequestID); !found {
		t.Fatal("pruning tombstones must keep the receipt")
	}
}

// Resolve reports the writer of record and tags the decision needs, content-free.
func TestPg_ErasureResolve_WriterAndTags(t *testing.T) {
	a, ks, er, ctx := newErasureStore(t)
	seedLifecycleDoc(t, a, ks, ctx, "lctest-r", "lctest-a", "r text", "dg-r", 1)
	execSQL(t, a, ctx, `UPDATE documents SET tags = '{team-x}' WHERE id = 'lctest-r'`)
	execSQL(t, a, ctx, `INSERT INTO chunks (id, text, metadata) VALUES ('lctest-loose', 'l',
		jsonb_build_object('written_by', 'lctest-b', 'tags', jsonb_build_array('ops')))`)
	cands, err := er.Resolve(ctx, domain.ErasureSelector{DocumentIDs: []string{"lctest-r", "lctest-none"},
		ChunkIDs: []string{"lctest-r-chunk-1", "lctest-loose"}})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]domain.ErasureCandidate{}
	for _, c := range cands {
		byID[c.Kind+":"+c.ID] = c
	}
	if len(cands) != 3 {
		t.Fatalf("candidates = %+v", cands)
	}
	if c := byID["document:lctest-r"]; c.WrittenBy != "lctest-a" || len(c.Tags) != 1 || c.Tags[0] != "team-x" {
		t.Fatalf("document candidate = %+v", c)
	}
	// ENG-1947: a chunk of a stored document resolves to the whole document (Via = the
	// chunk id), with the document's writer of record and authoritative tags.
	var viaChunk *domain.ErasureCandidate
	for i := range cands {
		if cands[i].Via == "lctest-r-chunk-1" {
			viaChunk = &cands[i]
		}
	}
	if c := viaChunk; c == nil || c.Kind != domain.ErasureTargetDocument || c.ID != "lctest-r" || c.DocumentID != "lctest-r" ||
		c.WrittenBy != "lctest-a" || len(c.Tags) != 1 || c.Tags[0] != "team-x" {
		t.Fatalf("chunk of a document = %+v, want its document", c)
	}
	if c := byID["chunk:lctest-loose"]; c.WrittenBy != "lctest-b" || len(c.Tags) != 1 || c.Tags[0] != "ops" {
		t.Fatalf("document-less chunk = %+v", c)
	}
}
