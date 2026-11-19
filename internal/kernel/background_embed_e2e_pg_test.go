package kernel

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/memory/forget"
)

// ENG-2143 end to end: a large ingest is accepted at once and embedded in the
// background, with visible progress, durable across a kernel death, fenced by erasure.
//
// Real path: a real, auto-migrated Postgres (PG_TEST_DSN), NewMemoryStack at the default
// configuration (derivation journal and background embedding on, min 256 chunks), the
// raw PgErasureStore under MemoryStack.Eraser() and the forget service over it,
// MemoryStack.Start / Shutdown, and IngestionManager.ProcessSync — the call both planes'
// IngestMemory make. Only the embedder is a fake: a deterministic hash embedder that
// takes a fixed time per text, so a 5 MB document costs what a remote embedder would.
//
//  1. Kernel A ingests a 5 MB document: the call returns well under the old ~190 s with
//     the document id and state accepted; the status advances accepted → embedding.
//  2. Kernel A dies mid-embed (its pool closed first, like a killed process). Kernel B
//     on the same database resumes where A stopped — it embeds only the rest — and the
//     document becomes searchable with every chunk stored once.
//  3. A second large document is forgotten while B embeds it: nothing of it remains
//     (document, chunks, entity, journal row), and the worker stops.
//  4. A small document is still stored with the call (searchable on return).
func TestPg_BackgroundEmbed_AcceptedResumedForgottenAndSmallStaysSync(t *testing.T) {
	cfg := pgTestConfig(t)
	cfg.Execution.Erasure.Enabled = true
	cfg.Execution.Erasure.AgentEraseEnabled = true
	cfg.Execution.Facts.ExtractEnabled = false
	cfg.Execution.Retrieval.HebbianEnabled = false
	cfg.Execution.Ingestion.DerivationRecoveryIntervalMs = 200
	ing := cfg.Execution.Ingestion
	if !ing.DerivationJournalEnabled || !ing.BackgroundEmbedEnabled || ing.BackgroundEmbedMinChunks != 256 {
		t.Fatalf("defaults: journal=%v background_embed=%v min_chunks=%d (owner decision 2026-11-19: on, 256)",
			ing.DerivationJournalEnabled, ing.BackgroundEmbedEnabled, ing.BackgroundEmbedMinChunks)
	}
	ctx := context.Background()
	stamp := strings.ReplaceAll(time.Now().UTC().Format("150405.000000"), ".", "")
	author := "eng2143-" + stamp
	alice := agentGateCtx(author)

	vecA, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPgVectorAdapter A: %v", err)
	}
	embA := newPaceEmbedder(cfg.Embedder.Dimensions, 4*time.Millisecond)
	memA, stopA := startEmbedStack(t, cfg, vecA, embA)

	vecB, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPgVectorAdapter B: %v", err)
	}
	t.Cleanup(vecB.Close)
	// Leave nothing in the shared scratch database: 10k chunks of the same boilerplate
	// would crowd other tests' recall-floor probes.
	clean := func() {
		c := context.Background()
		like := "ingest_memory://eng2143/%" + stamp + "%"
		for _, q := range []string{
			`DELETE FROM derivation_jobs WHERE document_id LIKE $1`,
			`DELETE FROM evidence WHERE source_key LIKE $1`,
			`DELETE FROM chunks WHERE document_id LIKE $1 OR id LIKE $1 OR id LIKE 'source_doc:' || $1`,
			`DELETE FROM documents WHERE id LIKE $1`,
		} {
			_, _ = vecB.Pool().Exec(c, q, like)
		}
	}
	t.Cleanup(clean)
	count := func(sql string, args ...any) int {
		t.Helper()
		var c int
		if err := vecB.Pool().QueryRow(ctx, sql, args...).Scan(&c); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return c
	}
	waitUntil := func(what string, limit time.Duration, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(limit)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}

	// ---- 1. a 5 MB document is accepted at once ----
	tok := "zqbig" + stamp
	big := domain.ExternalDocument{
		SourceURI: "ingest_memory://eng2143/big-" + stamp, SourceType: "file_drop", Title: "big " + tok,
		Body: megabytes(tok, 5), Author: author, Timestamp: time.Now().UTC(),
	}
	out := &domain.IngestOutcome{}
	started := time.Now()
	entity, err := memA.IngestionManager.ProcessSync(domain.WithIngestOutcome(alice, out), big)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("ProcessSync (5 MB): %v", err)
	}
	doc, total := out.DocumentID, out.Status.ChunksTotal
	t.Logf("5 MB ingest returned in %v: document %s, %d chunks, state %s", elapsed, doc, total, out.Status.State)
	if out.Status.State != domain.IngestAccepted || doc == "" || entity == "" || total < 1000 {
		t.Fatalf("outcome = %+v, entity %q: want accepted with the document id and its chunk count", out, entity)
	}
	// The old synchronous path took ~190 s for this size; the pace embedder alone would
	// take total*4ms. Accepting writes no chunk and embeds nothing.
	if limit := 60 * time.Second; elapsed > limit || elapsed > time.Duration(total)*4*time.Millisecond {
		t.Fatalf("the accepted ingest took %v (limit %v, embedding alone %v)", elapsed, limit, time.Duration(total)*4*time.Millisecond)
	}
	statusA := func() domain.IngestStatus {
		st, found, err := memA.IngestionManager.IngestStatus(ctx, doc)
		if err != nil || !found {
			t.Fatalf("IngestStatus = %+v, %v, %v", st, found, err)
		}
		return st
	}
	if st := statusA(); st.State != domain.IngestAccepted && st.State != domain.IngestEmbedding {
		t.Fatalf("status right after the call = %+v", st)
	}
	waitUntil("the background embed to advance", 60*time.Second, func() bool {
		st := statusA()
		return st.State == domain.IngestEmbedding && st.ChunksEmbedded >= 256 && st.ChunksEmbedded < total/2
	})
	st := statusA()
	if stored := count(`SELECT count(*) FROM chunks WHERE document_id = $1 AND id LIKE '%-chunk-%'`, doc); stored < st.ChunksEmbedded {
		t.Fatalf("status says %d embedded, the store holds %d chunks", st.ChunksEmbedded, stored)
	}

	// ---- 2. kernel A dies mid-embed; kernel B resumes ----
	vecA.Close()
	stopA()
	var remaining int
	if err := vecB.Pool().QueryRow(ctx, `SELECT cardinality(chunk_ids) FROM derivation_jobs WHERE kind = 'embed' AND document_id = $1`, doc).Scan(&remaining); err != nil {
		t.Fatalf("the embed job must survive kernel A: %v", err)
	}
	if remaining == 0 || remaining >= total {
		t.Fatalf("journaled remaining = %d of %d, want progress recorded", remaining, total)
	}
	embB := newPaceEmbedder(cfg.Embedder.Dimensions, time.Millisecond)
	memB, stopB := startEmbedStack(t, cfg, vecB, embB)
	t.Cleanup(stopB)
	waitUntil("kernel B to finish the document", 3*time.Minute, func() bool {
		s, _, _ := memB.IngestionManager.IngestStatus(ctx, doc)
		return s.State == domain.IngestSearchable
	})
	if c := count(`SELECT count(*) FROM chunks WHERE document_id = $1 AND id LIKE '%-chunk-%' AND embedding IS NOT NULL`, doc); c != total {
		t.Fatalf("searchable document holds %d embedded chunks, want %d", c, total)
	}
	// B resumed from the journal: it embedded the remaining chunks, not the document again
	// (a slice A stored but could not record is in "remaining" and re-embedded once).
	if n := int(embB.texts.Load()); n < remaining || n > remaining+cfg.Execution.Ingestion.BackgroundEmbedSliceChunks {
		t.Fatalf("kernel B embedded %d texts for %d remaining of %d: it did not resume", n, remaining, total)
	}
	if c := count(`SELECT count(*) FROM derivation_jobs WHERE kind = 'embed' AND document_id = $1`, doc); c != 0 {
		t.Fatalf("finished embed job left %d journal rows", c)
	}
	// Searchable through the enforcing read path: a vector search for the token finds it.
	qv, _ := embB.Embed(ctx, "paragraph "+tok+" ledger reconciliation")
	hits, err := memB.Reader().Search(alice, qv, domain.SearchOptions{TopK: 5, Scope: &domain.TagPredicate{Bypass: true}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	found := false
	for _, h := range hits {
		if strings.HasPrefix(h.Document.ID, doc+"-chunk-") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the searchable document is not found by a search for its token (%d hits)", len(hits))
	}

	// ---- 3. forget while embedding leaves nothing ----
	tok2 := "zqgone" + stamp
	gone := domain.ExternalDocument{
		SourceURI: "ingest_memory://eng2143/gone-" + stamp, SourceType: "file_drop", Title: "gone " + tok2,
		Body: megabytes(tok2, 1), Author: author, Timestamp: time.Now().UTC(),
	}
	embB.per.Store(int64(6 * time.Millisecond)) // slow enough to forget mid-way
	out2 := &domain.IngestOutcome{}
	entity2, err := memB.IngestionManager.ProcessSync(domain.WithIngestOutcome(alice, out2), gone)
	if err != nil || out2.Status.State != domain.IngestAccepted {
		t.Fatalf("ProcessSync (1 MB) = %+v, %v", out2, err)
	}
	doc2 := out2.DocumentID
	waitUntil("the second document to start embedding", 60*time.Second, func() bool {
		s, _, _ := memB.IngestionManager.IngestStatus(ctx, doc2)
		return s.State == domain.IngestEmbedding
	})
	svc := forget.New(memB.Eraser(), forget.Config{PreviewTTL: time.Minute, PolicyFingerprint: "eng2143"})
	if _, err := svc.Forget(alice, forget.Request{Selector: domain.ErasureSelector{DocumentIDs: []string{doc2}}}); err != nil {
		t.Fatalf("Forget mid-embed: %v", err)
	}
	doneBefore := memB.Derivations.Stats().EmbedDone
	waitUntil("the worker to stop", 60*time.Second, func() bool { return memB.Derivations.Stats().EmbedDone > doneBefore })
	time.Sleep(500 * time.Millisecond) // a late slice would land here
	for what, n := range map[string]int{
		"chunks":          count(`SELECT count(*) FROM chunks WHERE id LIKE $1 || '-chunk-%' OR document_id = $1`, doc2),
		"document row":    count(`SELECT count(*) FROM documents WHERE id = $1`, doc2),
		"entity":          count(`SELECT count(*) FROM chunks WHERE id = $1`, entity2),
		"journal rows":    count(`SELECT count(*) FROM derivation_jobs WHERE document_id = $1`, doc2),
		"chunks of token": count(`SELECT count(*) FROM chunks WHERE text LIKE '%' || $1 || '%'`, tok2),
	} {
		if n != 0 {
			t.Errorf("after forgetting an accepted document mid-embed: %d %s remain", n, what)
		}
	}
	if _, found, _ := memB.IngestionManager.IngestStatus(ctx, doc2); found {
		t.Error("a forgotten document must have no status")
	}

	// ---- 4. a small document is stored with the call ----
	out3 := &domain.IngestOutcome{}
	tok3 := "zqsmall" + stamp
	if _, err := memB.IngestionManager.ProcessSync(domain.WithIngestOutcome(alice, out3), domain.ExternalDocument{
		SourceURI: "ingest_memory://eng2143/small-" + stamp, SourceType: "file_drop", Title: "small " + tok3,
		Body: paragraphs(tok3, 3), Author: author, Timestamp: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("ProcessSync (small): %v", err)
	}
	if out3.Status.State != domain.IngestSearchable {
		t.Fatalf("small ingest status = %+v, want searchable on return", out3.Status)
	}
	if c := count(`SELECT count(*) FROM chunks WHERE document_id = $1 AND id LIKE '%-chunk-%' AND embedding IS NOT NULL`, out3.DocumentID); c == 0 {
		t.Fatal("a small document must be stored with the call")
	}
	if c := count(`SELECT count(*) FROM derivation_jobs WHERE kind = 'embed' AND document_id = $1`, out3.DocumentID); c != 0 {
		t.Fatal("a small document must not journal an embed job")
	}
}

// startEmbedStack composes and starts a memory stack the way the kernel does (erasure
// on, the raw erasure store under MemoryStack.Eraser()), with emb as the embedder.
func startEmbedStack(t *testing.T, cfg *config.Config, vec *postgres.PgVectorAdapter, emb domain.Embedder) (*MemoryStack, func()) {
	t.Helper()
	ks := postgres.NewPgKnowledgeStore(vec.Pool(), nil)
	lifecycle := postgres.NewPgDocumentLifecycle(ks)
	backend := NewMemoryBackend(vec)
	backend.Eraser = postgres.NewPgErasureStore(lifecycle)
	mem := NewMemoryStack(backend, nil, emb, cfg.Execution, nil, cfg.Chunker, nil,
		FactSubstrate{Documents: lifecycle}, RetrievalDeps{})
	if mem.Derivations == nil || mem.Eraser() == nil {
		t.Fatal("the composed stack must carry the derivation journal and the eraser")
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = mem.Start(runCtx)
	}()
	var once sync.Once
	return mem, func() {
		once.Do(func() {
			cancel()
			<-done
			mem.Shutdown(context.Background())
		})
	}
}

// paceEmbedder is hashEmbedder taking per for every text, like a remote embedder.
type paceEmbedder struct {
	dim   int
	per   atomic.Int64 // nanoseconds per text
	texts atomic.Int64
}

func newPaceEmbedder(dim int, per time.Duration) *paceEmbedder {
	p := &paceEmbedder{dim: dim}
	p.per.Store(int64(per))
	return p
}

func (p *paceEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	v, err := p.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return v[0], nil
}

func (p *paceEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	select {
	case <-time.After(time.Duration(len(texts)) * time.Duration(p.per.Load())):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	p.texts.Add(int64(len(texts)))
	out := make([][]float32, len(texts))
	for i, s := range texts {
		out[i], _ = hashEmbedder{dim: p.dim}.Embed(ctx, s)
	}
	return out, nil
}

// megabytes is about n MB of paragraphs naming tok.
func megabytes(tok string, n int) string {
	var b strings.Builder
	for b.Len() < n<<20 {
		b.WriteString(paragraphs(tok, 50))
	}
	return b.String()
}
