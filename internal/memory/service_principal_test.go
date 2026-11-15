package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// ENG-494 (owner decision 2026-11-15): every principal-less write runs as a
// deterministic service principal, and the operator can keep sources out of fact
// extraction (execution.memory.fact_extraction_sources).

// principalStore records the principal each chunk batch was saved under.
type principalStore struct {
	captureAllStore
	pmu        sync.Mutex
	principals []domain.PrincipalRef
}

func (p *principalStore) SaveBatch(ctx context.Context, docs []*domain.Document) error {
	p.pmu.Lock()
	p.principals = append(p.principals, domain.PrincipalFromContext(ctx))
	p.pmu.Unlock()
	return p.captureAllStore.SaveBatch(ctx, docs)
}

func (p *principalStore) seen() []domain.PrincipalRef {
	p.pmu.Lock()
	defer p.pmu.Unlock()
	return append([]domain.PrincipalRef(nil), p.principals...)
}

// /v1/ingest end to end through the batch lane: the token-authenticated push is
// written, on the batch worker's own context, as source:webhook:ingest — the
// principal travels with the queued document — and the chunks' writer of record is
// that source, not the Author the client put in the JSON body.
func TestWebhookIngest_BatchWorkerWritesAsTheSourcePrincipal(t *testing.T) {
	t.Parallel()
	emb := &mockEmbedder{vec: []float32{0.1, 0.2}}
	store := &principalStore{}
	im := NewIngestionManager(emb, &Agent{Manager: &MemoryManager{Store: store, Embedder: emb}},
		IngestionConfig{QueueSize: 10, BatchSize: 1, Workers: 1, BatchWait: 20 * time.Millisecond},
		config.ChunkerConfig{Default: "option_c"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	im.Start(ctx)

	h := NewWebhookReceiver("tok", im.Enqueue)
	doc := makeDoc(1)
	doc.Author = "agent-mallory" // a client-named writer of record
	body, _ := json.Marshal(doc)
	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", bytes.NewReader(body))
	req.Header.Set("X-Ingest-Token", "tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d", rec.Code)
	}

	deadline := time.Now().Add(3 * time.Second)
	for len(store.seen()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	ps := store.seen()
	if len(ps) == 0 {
		t.Fatal("the batch worker saved nothing")
	}
	for _, p := range ps {
		if p.String() != "source:webhook:ingest" {
			t.Fatalf("batch write principal %v, want source:webhook:ingest", p)
		}
	}
	checked := 0
	for _, d := range store.snapshot() {
		if strings.HasPrefix(d.ID, "source_doc:") {
			continue // the source-document entity, not a chunk
		}
		checked++
		if a, _ := d.Metadata["source_agent_id"].(string); a != "source:webhook:ingest" {
			t.Fatalf("chunk %s writer of record %q, want source:webhook:ingest", d.ID, a)
		}
	}
	if checked == 0 {
		t.Fatal("no chunk was saved")
	}
}

func TestFactSourceEligible(t *testing.T) {
	t.Parallel()
	tg := domain.SourcePrincipal("telegram", "bot1")
	slack := domain.SourcePrincipal("slack", "ws")
	alice := domain.AgentPrincipal("alice")
	cases := []struct {
		sources []string
		writer  domain.PrincipalRef
		want    bool
	}{
		{nil, tg, true},                             // default: everything (pre-guard behaviour)
		{nil, domain.PrincipalRef{}, true},          // default admits even an actorless ingest
		{[]string{"agent"}, alice, true},            // by kind
		{[]string{"agent"}, tg, false},              //
		{[]string{"source"}, slack, true},           // every connector
		{[]string{"source:telegram"}, tg, true},     // one connector
		{[]string{"source:telegram"}, slack, false}, //
		{[]string{"source:telegram"}, alice, false}, //
		{[]string{"user", "system"}, domain.SystemPrincipal, true},
		{[]string{"agent"}, domain.PrincipalRef{}, false}, // a list set: an actorless ingest matches nothing
	}
	for i, c := range cases {
		if got := factSourceEligible(c.sources, c.writer); got != c.want {
			t.Errorf("case %d: factSourceEligible(%v, %v) = %v, want %v", i, c.sources, c.writer, got, c.want)
		}
	}
}

// The guard is read on the ingest path: a source the operator left out is not
// queued for extraction (no LLM call), counted, and a sync wait settles at once.
func TestFactExtractor_EnqueueHonoursFactExtractionSources(t *testing.T) {
	t.Parallel()
	e := NewFactExtractor(nil, nil, nil, nil, FactExtractorConfig{ExtractionSources: []string{"source:telegram"}})
	chunks := []*domain.Document{{ID: "c1", Text: "I live in Paris."}}

	slackCtx := domain.WithPrincipal(context.Background(), domain.SourcePrincipal("slack", "ws"))
	w := e.Enqueue(slackCtx, "d-slack", domain.SourceMeta{}, chunks, true)
	if w == nil || !w.Done() || w.Failed() {
		t.Fatalf("a filtered source must settle its wait at once without failure: %+v", w)
	}
	tgCtx := domain.WithPrincipal(context.Background(), domain.SourcePrincipal("telegram", "bot1"))
	e.Enqueue(tgCtx, "d-tg", domain.SourceMeta{}, chunks, false)

	st := e.Stats()
	if st.SourceFiltered != 1 || st.Enqueued != 1 {
		t.Fatalf("stats %+v: want 1 filtered (slack), 1 enqueued (telegram)", st)
	}
}
