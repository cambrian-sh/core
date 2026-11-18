package kernel

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
	"github.com/cambrian-sh/core/internal/memory/forget"
)

// ENG-501 end to end (ADR-0137 D5 / ADR-0138 D3): a document is revised while the KG
// triplet job and the fact job of its PREVIOUS version are still in flight. The new
// version reuses the chunk ids, so without the version fence the old jobs would write
// the retired text's triplets and facts onto the new version's chunks, and the old fact
// job's extraction marker (keyed by the chunk ids) would make the new version's job skip.
//
// Real path: a real, auto-migrated Postgres (PG_TEST_DSN), NewMemoryStack with fact
// extraction on, the stack's own ChunkTripletsBatcher and FactExtractor wired onto the
// IngestionManager exactly as app/boot_server.go and NewMemoryStack do, and two
// ProcessSync ingests of one document. Only the model calls are fakes: a triplet
// extractor (the domain.TripletExtractor port production fills with the kg_extractor
// agent) and a fact generator, both of which hold the old version's call until the
// revision has landed.
func TestPg_DocumentRevision_StaleTripletAndFactJobsAreRefused(t *testing.T) {
	cfg := pgTestConfig(t)
	cfg.Execution.Erasure.Enabled = true
	cfg.Execution.Erasure.AgentEraseEnabled = true
	cfg.Execution.Facts.ExtractEnabled = true
	cfg.Execution.Facts.Workers = 1
	cfg.Execution.Facts.SupersessionEnabled = false
	cfg.Execution.Facts.AssessmentsEnabled = false
	cfg.Execution.Ingestion.SyncIngestWaitForKG = false
	cfg.Execution.Ingestion.SyncIngestWaitForFacts = false
	cfg.Execution.Ingestion.EdgeExtractionMaxIdleMs = 100
	cfg.Execution.Retrieval.HebbianEnabled = false
	ctx := context.Background()

	vec, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPgVectorAdapter: %v", err)
	}
	t.Cleanup(vec.Close)
	reg, err := domain.NewKindRegistry([]domain.KindSpec{domain.MemoryFactKindSpec()},
		[]domain.ResolutionAuthority{domain.ValidTimeSupersessionAuthority{}})
	if err != nil {
		t.Fatal(err)
	}
	ks := postgres.NewPgKnowledgeStore(vec.Pool(), reg)
	reader := postgres.NewPgFactItemReader(ks, "default", "rev-model", cfg.Embedder.Dimensions)
	backend := NewMemoryBackend(vec)
	lifecycle := postgres.NewPgDocumentLifecycle(ks)
	backend.Eraser = postgres.NewPgErasureStore(lifecycle)

	stamp := strings.ReplaceAll(time.Now().UTC().Format("150405.000000"), ".", "")
	tokV1, tokV2 := "zqrevone"+stamp, "zqrevtwo"+stamp
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	kgEntered, factEntered := make(chan struct{}), make(chan struct{})
	var kgOnce, factOnce sync.Once

	factGen := &revisionFactGen{tokV1: tokV1, tokV2: tokV2, release: release,
		entered: func() { factOnce.Do(func() { close(factEntered) }) }}
	emb := hashEmbedder{dim: cfg.Embedder.Dimensions}
	mem := NewMemoryStack(backend, nil, emb, cfg.Execution, nil, cfg.Chunker, factGen,
		FactSubstrate{Store: ks, Reader: reader, Inspector: reader, EmbedModelID: "rev-model", Documents: lifecycle}, RetrievalDeps{})
	if mem.ChunkTripletsBatcher == nil || mem.FactExtractor == nil {
		t.Fatal("the composed stack must carry the triplet batcher and the fact extractor")
	}
	mem.ChunkTripletsBatcher.UseExtractor(&revisionTripletExtractor{tokV1: tokV1, tokV2: tokV2, release: release,
		entered: func() { kgOnce.Do(func() { close(kgEntered) }) }})
	// app/boot_server.go: the ingest path feeds the stack's batcher.
	mem.IngestionManager.SetChunkTripletsBatcher(mem.ChunkTripletsBatcher)
	mem.ChunkTripletsBatcher.Start(ctx)
	mem.FactExtractor.Start(ctx)
	t.Cleanup(func() {
		unblock()
		mem.ChunkTripletsBatcher.Stop()
		mem.FactExtractor.Stop()
	})

	const author = "eng411d-rev"
	alice := agentGateCtx(author)
	uri := "ingest_memory://eng501/revised-" + stamp
	ingest := func(tok string) string {
		t.Helper()
		entity, err := mem.IngestionManager.ProcessSync(alice, domain.ExternalDocument{
			SourceURI: uri, SourceType: "file_drop", Title: "revised", Body: paragraphs(tok, 1), Author: author, Timestamp: time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("ProcessSync %s: %v", tok, err)
		}
		var doc string
		if err := vec.Pool().QueryRow(ctx, `SELECT document_id FROM chunks WHERE id = $1`, entity).Scan(&doc); err != nil {
			t.Fatalf("document of %s: %v", entity, err)
		}
		return doc
	}
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := vec.Pool().QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}
	waitFor := func(what string, ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(30 * time.Second):
			t.Fatalf("timed out waiting for %s", what)
		}
	}

	// Version 1: its triplet and fact jobs start and are held inside the model call.
	doc := ingest(tokV1)
	t.Cleanup(func() {
		svc := forget.New(mem.Eraser(), forget.Config{PreviewTTL: time.Minute, PolicyFingerprint: "eng501"})
		_, _ = svc.Forget(alice, forget.Request{Selector: domain.ErasureSelector{DocumentIDs: []string{doc}}})
		for _, q := range []string{
			`DELETE FROM memory_erasures WHERE principal = '` + author + `'`,
			`DELETE FROM document_versions WHERE document_id = '` + doc + `'`,
			`DELETE FROM chunks WHERE document_id = '` + doc + `'`,
			`DELETE FROM documents WHERE id = '` + doc + `'`,
		} {
			_, _ = vec.Pool().Exec(context.Background(), q)
		}
	})
	waitFor("the version-1 triplet job", kgEntered)
	waitFor("the version-1 fact job", factEntered)

	// Version 2 of the same document lands while they are held: same id, same chunk ids.
	if doc2 := ingest(tokV2); doc2 != doc {
		t.Fatalf("the revision must keep the document id: %q vs %q", doc2, doc)
	}
	if n := count(`SELECT version FROM documents WHERE id = $1`, doc); n != 2 {
		t.Fatalf("document version = %d, want 2", n)
	}
	chunkIDs := count(`SELECT count(*) FROM chunks WHERE document_id = $1 AND id NOT LIKE 'source_doc:%'`, doc)
	if chunkIDs == 0 {
		t.Fatal("version 2 stored no chunks")
	}

	// Release the old jobs. They drain first (FIFO, one batch / one worker), then v2's.
	unblock()
	deadline := time.Now().Add(30 * time.Second)
	for {
		v2Triplets := count(`SELECT count(*) FROM chunk_triplets ct JOIN chunks c ON c.id = ct.chunk_id
			WHERE c.document_id = $1 AND ct.h = $2`, doc, tokV2)
		v2Facts := count(`SELECT count(*) FROM statement_values WHERE value_text = $1`, tokV2)
		if v2Triplets > 0 && v2Facts > 0 && mem.ChunkTripletsBatcher.StaleVersionDrops() > 0 && mem.FactExtractor.Stats().StaleVersion > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("drain did not settle: v2 triplets=%d v2 facts=%d stale triplet drops=%d stale fact docs=%d",
				v2Triplets, v2Facts, mem.ChunkTripletsBatcher.StaleVersionDrops(), mem.FactExtractor.Stats().StaleVersion)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Nothing of version 1 reached version 2's chunks, its facts, or its marker.
	if n := count(`SELECT count(*) FROM chunk_triplets WHERE h = $1`, tokV1); n != 0 {
		t.Errorf("%d triplets of the retired version were written", n)
	}
	if n := count(`SELECT count(*) FROM statement_values WHERE value_text = $1`, tokV1); n != 0 {
		t.Errorf("%d fact values of the retired version were written", n)
	}
	if st := mem.FactExtractor.Stats(); st.StaleVersion != 1 || st.FailedDocs != 0 || st.Skipped != 0 {
		t.Errorf("fact extractor stats = %+v: the old job stops as stale, the new one is not skipped", st)
	}
	if n := count(`SELECT count(*) FROM knowledge_extractions WHERE document_id = $1 AND passages_covered = passages_total`, doc); n != 1 {
		t.Errorf("complete extraction markers of the document = %d, want version 2's one", n)
	}
	if failed, _ := mem.ChunkTripletsBatcher.FailureStats(); failed != 0 {
		t.Errorf("a retired version is not a KG failure: extractFailed = %d", failed)
	}
}

// revisionTripletExtractor returns one triplet per chunk, headed by the version token in
// its text, and holds a batch holding the version-1 token until release.
type revisionTripletExtractor struct {
	tokV1, tokV2 string
	release      <-chan struct{}
	entered      func()
}

func (e *revisionTripletExtractor) ExtractBatch(ctx context.Context, texts, _ []string) ([][]domain.ChunkTriplet, error) {
	for _, s := range texts {
		if strings.Contains(s, e.tokV1) {
			e.entered()
			select {
			case <-e.release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			break
		}
	}
	out := make([][]domain.ChunkTriplet, len(texts))
	for i, s := range texts {
		for _, tok := range []string{e.tokV1, e.tokV2} {
			if strings.Contains(s, tok) {
				out[i] = []domain.ChunkTriplet{{H: tok, R: "records", T: "reconciliation", Weight: 1}}
			}
		}
	}
	return out, nil
}

// revisionFactGen answers an extraction window with one fact quoting the version token
// of its first passage, and holds the version-1 call until release.
type revisionFactGen struct {
	tokV1, tokV2 string
	release      <-chan struct{}
	entered      func()
}

var revisionPassage = regexp.MustCompile(`(?m)^\[1\] `)

func (g *revisionFactGen) Generate(ctx context.Context, prompt string) (string, error) {
	if !revisionPassage.MatchString(prompt) {
		return `{"facts":[]}`, nil
	}
	tok := ""
	switch {
	case strings.Contains(prompt, g.tokV1):
		tok = g.tokV1
		g.entered()
		select {
		case <-g.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	case strings.Contains(prompt, g.tokV2):
		tok = g.tokV2
	default:
		return `{"facts":[]}`, nil
	}
	return fmt.Sprintf(`{"facts":[{"subject":"ledger","attribute":"named","value":%q,"passage":1,"quote":%q}]}`, tok, tok), nil
}
