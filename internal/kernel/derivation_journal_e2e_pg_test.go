package kernel

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
)

// ENG-2122 end to end: fact and KG triplet extraction survive a kernel death and an LLM
// outage.
//
// Real path: a real, auto-migrated Postgres (PG_TEST_DSN), NewMemoryStack with fact
// extraction on and the derivation journal at its default (on), the stack's own
// ChunkTripletsBatcher wired onto the IngestionManager as app/boot_server.go does,
// MemoryStack.Start / Shutdown, and ProcessSync ingests. Only the model calls are fakes.
//
// Kernel A ingests N documents while the fact provider is down for all but the first
// (each failed job is journaled for a retry) and the triplet extractor hangs (the KG
// jobs are queued in memory). Kernel A then dies: its pool is closed, so nothing it
// does afterwards reaches the database, exactly like a killed process. Kernel B — a
// fresh adapter and a fresh stack on the same database, with a working model — must
// recover every journaled job at boot: every document ends with its facts and triplets
// exactly once, the document A had already finished costs B no model call, and the
// journal is empty.
func TestPg_DerivationJournal_RestartAndLLMOutageLoseNoDerivation(t *testing.T) {
	cfg := pgTestConfig(t)
	cfg.Execution.Facts.ExtractEnabled = true
	cfg.Execution.Facts.Workers = 1
	cfg.Execution.Facts.SupersessionEnabled = false
	cfg.Execution.Facts.AssessmentsEnabled = false
	cfg.Execution.Ingestion.SyncIngestWaitForKG = false
	cfg.Execution.Ingestion.SyncIngestWaitForFacts = false
	cfg.Execution.Ingestion.EdgeExtractionMaxIdleMs = 50
	cfg.Execution.Ingestion.KgTripletsMaxRequeues = 0
	cfg.Execution.Ingestion.DerivationRecoveryIntervalMs = 200
	// A failed job waits an hour in kernel A: only the restart can bring it back here.
	cfg.Execution.Ingestion.DerivationRetryBaseMs = 3600000
	cfg.Execution.Ingestion.DerivationRetryMaxMs = 3600000
	cfg.Execution.Retrieval.HebbianEnabled = false
	if !cfg.Execution.Ingestion.DerivationJournalEnabled {
		t.Fatal("derivation_journal_enabled must default to true (owner decision 2026-11-18)")
	}
	ctx := context.Background()

	stamp := strings.ReplaceAll(time.Now().UTC().Format("150405.000000"), ".", "")
	const n = 4
	toks := make([]string, n)
	for i := range toks {
		toks[i] = fmt.Sprintf("zqjrnl%c%s", 'a'+i, stamp)
	}
	// The run leaves nothing in the shared database (ENG-2181): its four documents carry
	// the paragraphs() boilerplate other tests rank against (TestPg_Forget_QueryPreview).
	t.Cleanup(func() { cleanJournalRun(t, toks, stamp) })

	// ---- kernel A: the provider is down for every document but the first; the
	// triplet extractor hangs until A is dead, then fails. ----
	vecA, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPgVectorAdapter A: %v", err)
	}
	hang := make(chan struct{})
	var hangOnce sync.Once
	release := func() { hangOnce.Do(func() { close(hang) }) }
	t.Cleanup(release)
	kgEntered := make(chan struct{})
	var kgOnce sync.Once
	genA := &journalFactGen{toks: toks, down: func(tok string) bool { return tok != toks[0] }}
	extA := &journalTripletExtractor{toks: toks, hold: func(ctx context.Context) error {
		kgOnce.Do(func() { close(kgEntered) })
		select {
		case <-hang:
		case <-ctx.Done():
		}
		return errors.New("kernel A died")
	}}
	memA, stopA := startJournalStack(t, cfg, vecA, genA, extA)

	author := "eng2122-" + stamp
	alice := agentGateCtx(author)
	docs := make([]string, n)
	for i, tok := range toks {
		entity, err := memA.IngestionManager.ProcessSync(alice, domain.ExternalDocument{
			SourceURI: "ingest_memory://eng2122/doc-" + tok, SourceType: "file_drop", Title: "journal " + tok,
			Body: paragraphs(tok, 2), Author: author, Timestamp: time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("ProcessSync %d: %v", i, err)
		}
		if err := vecA.Pool().QueryRow(ctx, `SELECT document_id FROM chunks WHERE id = $1`, entity).Scan(&docs[i]); err != nil {
			t.Fatalf("document of %s: %v", entity, err)
		}
	}

	vecB, err := postgres.NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		t.Fatalf("NewPgVectorAdapter B: %v", err)
	}
	t.Cleanup(vecB.Close)
	count := func(sql string, args ...any) int {
		t.Helper()
		var c int
		if err := vecB.Pool().QueryRow(ctx, sql, args...).Scan(&c); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return c
	}
	waitUntil := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(60 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	// Every ingest journaled both kinds before its chunks were saved.
	if c := count(`SELECT count(*) FROM derivation_jobs WHERE document_id = ANY($1) AND kind = 'kg_triplets'`, docs); c != n {
		t.Fatalf("journaled KG jobs = %d, want %d", c, n)
	}
	// A: the first document's facts land and its fact job leaves the journal; the
	// others fail once and wait out their backoff; the KG batch hangs.
	waitUntil("kernel A's fact jobs to settle", func() bool {
		return count(`SELECT count(*) FROM derivation_jobs WHERE document_id = ANY($1) AND kind = 'facts' AND attempts = 1 AND next_attempt_at > now() + interval '30 minutes'`, docs) == n-1 &&
			count(`SELECT count(*) FROM derivation_jobs WHERE document_id = $1 AND kind = 'facts'`, docs[0]) == 0
	})
	select {
	case <-kgEntered:
	case <-time.After(30 * time.Second):
		t.Fatal("kernel A's triplet batch never started")
	}
	// The first document's uninterrupted run is the reference: one fact per extraction
	// window (every document has the same shape).
	wantFacts := count(`SELECT count(*) FROM statement_values WHERE predicate = 'value' AND value_text = $1`, toks[0])
	windows := genA.extracted(toks[0])
	if wantFacts == 0 || wantFacts != windows {
		t.Fatalf("kernel A stored %d facts for the first document over %d windows, want one per window", wantFacts, windows)
	}
	if c := count(`SELECT count(*) FROM chunk_triplets WHERE h = ANY($1)`, toks); c != 0 {
		t.Fatalf("kernel A stored %d triplets before dying, want 0", c)
	}
	callsA := genA.calls.Load()

	// ---- kernel A dies: its pool goes first, then its hung call returns. ----
	vecA.Close()
	release()
	stopA()

	// ---- kernel B: same database, working model. Its first recovery pass takes every
	// job A left behind. ----
	genB := &journalFactGen{toks: toks, down: func(string) bool { return false }}
	extB := &journalTripletExtractor{toks: toks}
	memB, stopB := startJournalStack(t, cfg, vecB, genB, extB)
	t.Cleanup(stopB)
	if memB.Derivations == nil {
		t.Fatal("the composed stack must carry the derivation journal")
	}
	waitUntil("kernel B to drain the journal", func() bool {
		return count(`SELECT count(*) FROM derivation_jobs WHERE document_id = ANY($1)`, docs) == 0
	})

	for i, tok := range toks {
		if c := count(`SELECT count(*) FROM statement_values WHERE predicate = 'value' AND value_text = $1`, tok); c != wantFacts {
			t.Errorf("document %d: %d fact values, want exactly %d (one per window, once)", i, c, wantFacts)
		}
		chunks := count(`SELECT count(*) FROM chunks WHERE document_id = $1 AND id NOT LIKE 'source_doc:%'`, docs[i])
		if c := count(`SELECT count(*) FROM chunk_triplets WHERE h = $1`, tok); chunks == 0 || c != chunks {
			t.Errorf("document %d: %d triplets over %d chunks, want exactly one per chunk", i, c, chunks)
		}
		if c := count(`SELECT count(*) FROM knowledge_extractions WHERE document_id = $1 AND passages_covered = passages_total`, docs[i]); c != 1 {
			t.Errorf("document %d: %d complete extraction markers, want 1", i, c)
		}
	}
	if got := genB.extracted(toks[0]); got != 0 {
		t.Errorf("kernel B called the model %d times for the document A had finished", got)
	}
	for _, tok := range toks[1:] {
		if got := genB.extracted(tok); got != windows {
			t.Errorf("kernel B extraction calls for %s = %d, want %d (one per window)", tok, got, windows)
		}
	}
	if st := memB.Derivations.Stats(); st.Recovered < uint64(2*n-1) || st.Completed < uint64(2*n-1) {
		t.Errorf("kernel B journal stats = %+v: want every orphaned job recovered and completed", st)
	}
	if callsA == 0 {
		t.Error("kernel A made no fact call")
	}
}

// startJournalStack composes and starts a memory stack the way the kernel does, with
// the fakes in the model seats, and returns its stop function.
func startJournalStack(t *testing.T, cfg *config.Config, vec *postgres.PgVectorAdapter, gen domain.Generator, ext domain.TripletExtractor) (*MemoryStack, func()) {
	t.Helper()
	reg, err := domain.NewKindRegistry([]domain.KindSpec{domain.MemoryFactKindSpec()},
		[]domain.ResolutionAuthority{domain.ValidTimeSupersessionAuthority{}})
	if err != nil {
		t.Fatal(err)
	}
	ks := postgres.NewPgKnowledgeStore(vec.Pool(), reg)
	reader := postgres.NewPgFactItemReader(ks, "default", "jrnl-model", cfg.Embedder.Dimensions)
	backend := NewMemoryBackend(vec)
	lifecycle := postgres.NewPgDocumentLifecycle(ks)
	mem := NewMemoryStack(backend, nil, hashEmbedder{dim: cfg.Embedder.Dimensions}, cfg.Execution, nil, cfg.Chunker, gen,
		FactSubstrate{Store: ks, Reader: reader, Inspector: reader, EmbedModelID: "jrnl-model", Documents: lifecycle}, RetrievalDeps{})
	if mem.ChunkTripletsBatcher == nil || mem.FactExtractor == nil {
		t.Fatal("the composed stack must carry the triplet batcher and the fact extractor")
	}
	mem.ChunkTripletsBatcher.UseExtractor(ext) // app/boot_tools.go, before Start
	mem.IngestionManager.SetChunkTripletsBatcher(mem.ChunkTripletsBatcher)
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = mem.Start(runCtx) // blocks in the memory worker until runCtx ends
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

// journalFactGen answers an extraction window with one fact quoting the token of the
// document it was sent; down(tok) makes the provider fail that document's call.
type journalFactGen struct {
	toks  []string
	down  func(tok string) bool
	calls atomic.Int64
	mu    sync.Mutex
	per   map[string]int
}

func (g *journalFactGen) Generate(_ context.Context, prompt string) (string, error) {
	if !revisionPassage.MatchString(prompt) {
		return `{"facts":[]}`, nil
	}
	for _, tok := range g.toks {
		if !strings.Contains(prompt, tok) {
			continue
		}
		g.calls.Add(1)
		g.mu.Lock()
		if g.per == nil {
			g.per = map[string]int{}
		}
		g.per[tok]++
		g.mu.Unlock()
		if g.down(tok) {
			return "", errors.New("provider 503: service unavailable")
		}
		return fmt.Sprintf(`{"facts":[{"subject":"ledger","attribute":"named","value":%q,"passage":1,"quote":%q}]}`, tok, tok), nil
	}
	return `{"facts":[]}`, nil
}

func (g *journalFactGen) extracted(tok string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.per[tok]
}

// journalTripletExtractor returns one triplet per chunk headed by its document's token
// (read from the chunk id, which names the document's source URI);
// hold, when set, runs before every batch and its error fails the batch.
type journalTripletExtractor struct {
	toks []string
	hold func(ctx context.Context) error
}

func (e *journalTripletExtractor) ExtractBatch(ctx context.Context, texts, ids []string) ([][]domain.ChunkTriplet, error) {
	if e.hold != nil {
		if err := e.hold(ctx); err != nil {
			return nil, err
		}
	}
	out := make([][]domain.ChunkTriplet, len(texts))
	for i := range texts {
		for _, tok := range e.toks {
			if i < len(ids) && strings.Contains(ids[i], tok) {
				out[i] = []domain.ChunkTriplet{{H: tok, R: "records", T: "reconciliation", Weight: 1}}
			}
		}
	}
	return out, nil
}

// cleanJournalRun deletes everything one run of the journal E2E wrote, by its stamp
// (document ids are ingest_memory://eng2122/doc-zqjrnl<letter><stamp>…) and its tokens
// (the fact values and triplet heads), and fails the test if any of it remains: a
// shared PG_TEST_DSN (the hq gate DB) must not accumulate this run's chunks, whose
// boilerplate crowds other tests' recall (ENG-2181).
func cleanJournalRun(t *testing.T, toks []string, stamp string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("PG_TEST_DSN"))
	if err != nil {
		t.Errorf("cleanup: connect: %v", err)
		return
	}
	defer conn.Close(ctx)
	like := "ingest_memory://eng2122/doc-zqjrnl_" + stamp + "%"
	items := `SELECT item_id FROM statement_values WHERE value_text = ANY($1)`
	for _, q := range []struct {
		sql string
		arg any
	}{
		{`DELETE FROM derivation_jobs WHERE document_id LIKE $1`, like},
		{`DELETE FROM chunk_triplets WHERE h = ANY($1)`, toks},
		{`DELETE FROM knowledge_extractions WHERE document_id LIKE $1`, like},
		{`DELETE FROM resolutions WHERE item_id IN (` + items + `)`, toks},
		{`DELETE FROM knowledge_items WHERE id IN (` + items + `)`, toks},
		{`DELETE FROM evidence WHERE source_key LIKE $1`, like},
		{`DELETE FROM chunks WHERE document_id LIKE $1 OR id LIKE 'source_doc:' || $1`, like},
		{`DELETE FROM documents WHERE id LIKE $1`, like},
	} {
		if _, err := conn.Exec(ctx, q.sql, q.arg); err != nil {
			t.Errorf("cleanup: %s: %v", q.sql, err)
		}
	}
	var left int
	if err := conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM chunks WHERE document_id LIKE $1 OR id LIKE 'source_doc:' || $1)
		+ (SELECT count(*) FROM documents WHERE id LIKE $1) + (SELECT count(*) FROM derivation_jobs WHERE document_id LIKE $1)
		+ (SELECT count(*) FROM statement_values WHERE value_text = ANY($2))`, like, toks).Scan(&left); err != nil {
		t.Errorf("cleanup: count: %v", err)
	} else if left != 0 {
		t.Errorf("cleanup left %d rows of this run in the database", left)
	}
}
