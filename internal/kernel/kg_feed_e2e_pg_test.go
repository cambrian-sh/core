package kernel

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
)

// ENG-2111 end to end: one huge ingest neither starves nor drops other documents'
// enrichment, and a document with more chunks than the KG queue holds finishes.
//
// Real path: a real, auto-migrated Postgres (PG_TEST_DSN), NewMemoryStack with fact
// extraction on and the derivation journal at its default (on), the stack's own
// ChunkTripletsBatcher wired onto the IngestionManager as the boot path does,
// MemoryStack.Start / Shutdown, and ProcessSync ingests. Only the model calls are fakes;
// the triplet extractor takes a little time per batch, like a real one.
//
// The KG queue holds 32 chunks; the huge document has many times that. It is ingested
// first, then three small documents. Before ENG-2111 the huge document filled the
// queue: its tail and the small documents met "queue full" (deferred by ENG-2122, so
// every one waited for retries behind it), and the huge one could never finish. Now:
// the small documents' triplets land while most of the huge document is still to do,
// their facts land, the huge document's triplets all land, the batcher drops nothing,
// and the journal is empty at the end.
func TestPg_KGFeed_HugeIngestDoesNotStarveOrDropOtherDocuments(t *testing.T) {
	cfg := pgTestConfig(t)
	cfg.Execution.Facts.ExtractEnabled = true
	cfg.Execution.Facts.SupersessionEnabled = false
	cfg.Execution.Facts.AssessmentsEnabled = false
	cfg.Execution.Ingestion.SyncIngestWaitForKG = false
	cfg.Execution.Ingestion.SyncIngestWaitForFacts = false
	cfg.Execution.Ingestion.EdgeExtractionQueueSize = 32
	cfg.Execution.Ingestion.EdgeExtractionBatchSize = 8
	cfg.Execution.Ingestion.EdgeExtractionMaxIdleMs = 20
	cfg.Execution.Ingestion.KgTripletsMaxRequeues = 0
	cfg.Execution.Ingestion.DerivationKGSliceChunks = 8
	cfg.Execution.Retrieval.HebbianEnabled = false
	if d := cfg.Execution.Ingestion; !d.DerivationJournalEnabled || d.DerivationKGSlicesInFlight != 2 {
		t.Fatalf("journal on with 2 slices in flight by default; got %+v", d)
	}
	ctx := context.Background()

	stamp := strings.ReplaceAll(time.Now().UTC().Format("150405.000000"), ".", "")
	huge := "zqhuge" + stamp
	smalls := []string{"zqsmla" + stamp, "zqsmlb" + stamp, "zqsmlc" + stamp}
	toks := append([]string{huge}, smalls...)

	vec, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPgVectorAdapter: %v", err)
	}
	t.Cleanup(vec.Close)
	gen := &journalFactGen{toks: toks, down: func(string) bool { return false }}
	ext := &orderedTripletExtractor{journalTripletExtractor: journalTripletExtractor{toks: toks}, delay: 15 * time.Millisecond}
	mem, stop := startJournalStack(t, cfg, vec, gen, ext)
	t.Cleanup(stop)

	author := "eng2111-" + stamp
	alice := agentGateCtx(author)
	docs := make([]string, len(toks))
	for i, tok := range toks {
		n := 2
		if tok == huge {
			n = 300
		}
		entity, err := mem.IngestionManager.ProcessSync(alice, domain.ExternalDocument{
			SourceURI: "ingest_memory://eng2111/doc-" + tok, SourceType: "file_drop", Title: "feed " + tok,
			Body: manifestParagraphs(tok, n), Author: author, Timestamp: time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("ProcessSync %s: %v", tok, err)
		}
		if err := vec.Pool().QueryRow(ctx, `SELECT document_id FROM chunks WHERE id = $1`, entity).Scan(&docs[i]); err != nil {
			t.Fatalf("document of %s: %v", entity, err)
		}
	}
	// Leave no rows for the other Pg tests sharing this database (600 chunks would crowd
	// their searches).
	t.Cleanup(func() {
		for _, q := range []struct {
			sql string
			arg []string
		}{
			{`DELETE FROM chunk_triplets WHERE h = ANY($1)`, toks},
			{`DELETE FROM derivation_jobs WHERE document_id = ANY($1)`, docs},
			{`DELETE FROM chunks WHERE document_id = ANY($1)`, docs},
		} {
			if _, err := vec.Pool().Exec(context.Background(), q.sql, q.arg); err != nil {
				t.Logf("cleanup %q: %v", q.sql, err)
			}
		}
	})
	count := func(sql string, args ...any) int {
		t.Helper()
		var c int
		if err := vec.Pool().QueryRow(ctx, sql, args...).Scan(&c); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return c
	}
	chunksOf := func(doc string) int {
		return count(`SELECT count(*) FROM chunks WHERE document_id = $1 AND id NOT LIKE 'source_doc:%'`, doc)
	}
	hugeChunks := chunksOf(docs[0])
	if hugeChunks <= 4*cfg.Execution.Ingestion.EdgeExtractionQueueSize {
		t.Fatalf("the huge document has %d chunks; it must be several times the KG queue (%d)", hugeChunks, cfg.Execution.Ingestion.EdgeExtractionQueueSize)
	}
	deadline := time.Now().Add(120 * time.Second)
	for count(`SELECT count(*) FROM derivation_jobs WHERE document_id = ANY($1)`, docs) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("journal not drained: %d rows left", count(`SELECT count(*) FROM derivation_jobs WHERE document_id = ANY($1)`, docs))
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Fairness: when the last small-document chunk was extracted, most of the huge
	// document was still to do.
	hugeBefore := ext.extractedBefore(huge, smalls)
	t.Logf("huge document: %d chunks (KG queue %d); %d of them extracted before the last small-document chunk",
		hugeChunks, cfg.Execution.Ingestion.EdgeExtractionQueueSize, hugeBefore)
	if hugeBefore >= hugeChunks/2 {
		t.Errorf("the small documents' triplets waited until %d of %d huge-document chunks were done", hugeBefore, hugeChunks)
	}
	// Completeness: every chunk of every document has its triplet, exactly once; the
	// small documents have their facts.
	for i, tok := range toks {
		chunks := chunksOf(docs[i])
		if c := count(`SELECT count(*) FROM chunk_triplets WHERE h = $1`, tok); chunks == 0 || c != chunks {
			t.Errorf("%s: %d triplets over %d chunks, want one per chunk", tok, c, chunks)
		}
		if tok != huge && count(`SELECT count(*) FROM statement_values WHERE predicate = 'value' AND value_text = $1`, tok) == 0 {
			t.Errorf("%s: no facts", tok)
		}
	}
	if _, dropped, _, _, _ := mem.ChunkTripletsBatcher.Stats(); dropped != 0 {
		t.Errorf("the KG batcher dropped %d chunks", dropped)
	}
	st := mem.Derivations.Stats()
	t.Logf("journal stats: %+v", st)
	if st.Deferred != 0 || st.Failed != 0 || st.KGSlices < uint64(hugeChunks/8) || st.KGAdvanced == 0 {
		t.Errorf("journal stats = %+v: want no deferral or failure, the huge document fed in slices with progress written", st)
	}
}

// manifestParagraphs is paragraphs() in a vocabulary of its own, so this test's
// documents never compete with the forget tests' "ledger" documents in a search.
func manifestParagraphs(tok string, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "Section %d. The %s manifest lists shipping crate %d for the harbour depot. ", i, tok, i)
		b.WriteString(strings.Repeat("Routine stevedore remarks about crate handling are noted here. ", 18))
		b.WriteString("\n\n")
	}
	return b.String()
}

// orderedTripletExtractor is journalTripletExtractor that takes delay per batch and
// records the token of every chunk it extracted, in order.
type orderedTripletExtractor struct {
	journalTripletExtractor
	delay time.Duration
	mu    sync.Mutex
	order []string
}

func (e *orderedTripletExtractor) ExtractBatch(ctx context.Context, texts, ids []string) ([][]domain.ChunkTriplet, error) {
	time.Sleep(e.delay)
	out, err := e.journalTripletExtractor.ExtractBatch(ctx, texts, ids)
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, id := range ids {
		for _, tok := range e.toks {
			if strings.Contains(id, tok) {
				e.order = append(e.order, tok)
			}
		}
	}
	return out, err
}

// extractedBefore is how many chunks of tok were extracted before the last chunk of
// any of others.
func (e *orderedTripletExtractor) extractedBefore(tok string, others []string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	last := -1
	for i, t := range e.order {
		for _, o := range others {
			if t == o {
				last = i
			}
		}
	}
	if last < 0 {
		panic(fmt.Sprintf("none of %v was extracted", others))
	}
	n := 0
	for _, t := range e.order[:last] {
		if t == tok {
			n++
		}
	}
	return n
}
