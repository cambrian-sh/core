package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// ENG-2143 unit tests: a large ProcessSync is accepted and embedded in the background
// by the derivation journal; a small one stays synchronous. The Postgres path (fences,
// restart, erasure, the status read) is proven in internal/kernel (TestPg_BackgroundEmbed_*).

type stubDocumentStore struct{}

func (stubDocumentStore) SaveDocument(_ context.Context, d domain.SourceDocument) ([]string, error) {
	return d.Tags, nil
}

// failingStore fails SaveBatch with err from the call numbered failAt (1-based).
type failingStore struct {
	captureAllStore
	calls  atomic.Int32
	failAt int32
	err    error
}

func (s *failingStore) SaveBatch(ctx context.Context, docs []*domain.Document) error {
	if s.calls.Add(1) >= s.failAt && s.err != nil {
		return s.err
	}
	return s.captureAllStore.SaveBatch(ctx, docs)
}

func backgroundTestManager(t *testing.T, store domain.VectorStore, minChunks, slice int) (*IngestionManager, *fakeJournal, *DerivationJournaler) {
	t.Helper()
	agent := &Agent{Manager: &MemoryManager{Store: store, Embedder: &mockEmbedder{vec: []float32{0.1, 0.2}}}}
	reg, err := NewRegistry(defaultChunkers(), config.ChunkerConfig{Default: "option_c"})
	if err != nil {
		t.Fatal(err)
	}
	im := NewIngestionManagerWithRegistry(&mockEmbedder{vec: []float32{0.1, 0.2}}, agent, reg,
		IngestionConfig{BackgroundEmbedMinChunks: minChunks})
	im.SetDocumentStore(stubDocumentStore{})
	fj := newFakeJournal()
	d := NewDerivationJournaler(fj, journalReader(nil), nil, nil, DerivationJournalConfig{
		BackgroundEmbed: true, EmbedSliceChunks: slice, EmbedWorkers: 1, RetryBase: time.Minute, RetryMax: time.Hour,
	})
	if d == nil {
		t.Fatal("a journal with background embedding on must exist without facts or KG")
	}
	im.SetDerivationJournal(d)
	t.Cleanup(d.Stop)
	return im, fj, d
}

func paragraphDoc(n int) domain.ExternalDocument {
	doc := makeDoc(1)
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "Paragraph %d about the harbour ledger and its quarterly reconciliation.\n\n", i)
	}
	doc.Body = b.String()
	doc.Tags = []string{"finance"}
	return doc
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestBackgroundEmbed_LargeDocumentAcceptedThenSearchable(t *testing.T) {
	store := &captureAllStore{}
	im, fj, d := backgroundTestManager(t, store, 3, 2)
	out := &domain.IngestOutcome{}
	ctx := domain.WithIngestOutcome(domain.WithPrincipal(context.Background(), domain.AgentPrincipal("alice")), out)

	entity, err := im.ProcessSync(ctx, paragraphDoc(5))
	if err != nil || entity == "" {
		t.Fatalf("ProcessSync = %q, %v", entity, err)
	}
	if n := len(store.facts()); n != 0 {
		t.Fatalf("an accepted document stored %d chunks with the call, want 0", n)
	}
	if out.Status.State != domain.IngestAccepted || out.Status.ChunksTotal != 5 || out.DocumentID == "" {
		t.Fatalf("outcome = %+v, want accepted with 5 chunks and a document id", out)
	}
	row, ok := fj.row(domain.DerivationEmbed, out.DocumentID)
	if !ok || row.job.ChunksTotal != 5 || len(row.job.ChunkIDs) != 5 || len(row.job.Staged) == 0 {
		t.Fatalf("embed row = %+v (ok=%v), want 5 staged chunks", row.job, ok)
	}
	if row.job.Writer != domain.AgentPrincipal("alice") {
		t.Fatalf("journaled writer = %+v", row.job.Writer)
	}
	if st, _, _ := im.IngestStatus(ctx, out.DocumentID); st.State != domain.IngestAccepted {
		t.Fatalf("status before the worker = %+v, want accepted", st)
	}

	d.RecoverOnce(context.Background())
	waitFor(t, "the embed row to complete", func() bool {
		_, ok := fj.row(domain.DerivationEmbed, out.DocumentID)
		return !ok
	})
	facts := store.facts()
	if len(facts) != 5 {
		t.Fatalf("stored %d chunks, want 5", len(facts))
	}
	for _, f := range facts {
		if len(f.Embedding.Vector) == 0 {
			t.Fatalf("chunk %s stored without a vector", f.ID)
		}
		if tags, ok := f.Metadata["tags"].([]string); !ok || len(tags) != 1 || tags[0] != "finance" {
			t.Fatalf("chunk %s tags = %#v, want []string{finance}", f.ID, f.Metadata["tags"])
		}
	}
	if fj.advances != 3 { // 5 chunks in slices of 2
		t.Fatalf("progress writes = %d, want 3", fj.advances)
	}
	if st, _, _ := im.IngestStatus(ctx, out.DocumentID); st.State != domain.IngestSearchable {
		t.Fatalf("status after = %+v, want searchable", st)
	}
	if s := d.Stats(); s.EmbedAccepted != 1 || s.EmbedDone != 1 || s.EmbedSlices != 3 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestBackgroundEmbed_SmallDocumentStaysSynchronous(t *testing.T) {
	store := &captureAllStore{}
	im, fj, _ := backgroundTestManager(t, store, 10, 2)
	out := &domain.IngestOutcome{}
	ctx := domain.WithIngestOutcome(domain.WithPrincipal(context.Background(), domain.AgentPrincipal("alice")), out)
	if _, err := im.ProcessSync(ctx, paragraphDoc(3)); err != nil {
		t.Fatal(err)
	}
	if n := len(store.facts()); n != 3 {
		t.Fatalf("a small document stored %d chunks with the call, want 3", n)
	}
	if out.Status.State != domain.IngestSearchable {
		t.Fatalf("status = %+v, want searchable", out.Status)
	}
	if _, ok := fj.row(domain.DerivationEmbed, out.DocumentID); ok {
		t.Fatal("a small document must not journal an embed job")
	}
}

// A slice refused by the erasure fence ends the job: nothing more is stored and the
// row of that version is gone.
func TestBackgroundEmbed_ErasedMidwayStops(t *testing.T) {
	store := &failingStore{failAt: 2, err: fmt.Errorf("fence: %w", domain.ErrSourceErased)}
	im, fj, d := backgroundTestManager(t, store, 3, 2)
	out := &domain.IngestOutcome{}
	ctx := domain.WithIngestOutcome(domain.WithPrincipal(context.Background(), domain.AgentPrincipal("alice")), out)
	if _, err := im.ProcessSync(ctx, paragraphDoc(6)); err != nil {
		t.Fatal(err)
	}
	d.RecoverOnce(context.Background())
	waitFor(t, "the job to stop", func() bool {
		_, ok := fj.row(domain.DerivationEmbed, out.DocumentID)
		return !ok
	})
	if n := len(store.facts()); n != 2 {
		t.Fatalf("stored %d chunks, want only the first slice (2)", n)
	}
	if store.calls.Load() != 2 {
		t.Fatalf("SaveBatch calls = %d, want 2 (no slice after the refusal)", store.calls.Load())
	}
}

// A renewal that finds the row gone (erased, or replaced by a revise, while the slice
// was being stored) stops the worker after that slice.
func TestBackgroundEmbed_RowGoneStops(t *testing.T) {
	store := &hookStore{}
	im, fj, d := backgroundTestManager(t, store, 3, 2)
	out := &domain.IngestOutcome{}
	ctx := domain.WithIngestOutcome(domain.WithPrincipal(context.Background(), domain.AgentPrincipal("alice")), out)
	if _, err := im.ProcessSync(ctx, paragraphDoc(6)); err != nil {
		t.Fatal(err)
	}
	store.after = func() { // the erase lands while the first slice is stored
		fj.mu.Lock()
		delete(fj.rows, jkey(domain.DerivationEmbed, out.DocumentID))
		fj.mu.Unlock()
	}
	d.RecoverOnce(context.Background())
	waitFor(t, "the worker to stop", func() bool { return d.Stats().EmbedDone == 1 })
	if n := store.calls.Load(); n != 1 {
		t.Fatalf("SaveBatch calls = %d, want 1 (the worker stops at the renewal)", n)
	}
}

type hookStore struct {
	captureAllStore
	calls atomic.Int32
	after func()
}

func (s *hookStore) SaveBatch(ctx context.Context, docs []*domain.Document) error {
	s.calls.Add(1)
	err := s.captureAllStore.SaveBatch(ctx, docs)
	if s.after != nil {
		s.after()
	}
	return err
}

// An embedder that returns nothing fails the attempt; the job is retried with backoff
// and its status carries the reason.
func TestBackgroundEmbed_EmbedderOutageRetriesWithReason(t *testing.T) {
	store := &captureAllStore{}
	im, fj, d := backgroundTestManager(t, store, 3, 2)
	out := &domain.IngestOutcome{}
	ctx := domain.WithIngestOutcome(domain.WithPrincipal(context.Background(), domain.AgentPrincipal("alice")), out)
	if _, err := im.ProcessSync(ctx, paragraphDoc(4)); err != nil {
		t.Fatal(err)
	}
	im.embedder = &mockEmbedder{vec: nil} // the outage begins after acceptance
	d.RecoverOnce(context.Background())
	waitFor(t, "the failed attempt to be rescheduled", func() bool {
		r, ok := fj.row(domain.DerivationEmbed, out.DocumentID)
		return ok && r.attempts == 1
	})
	st, _, _ := im.IngestStatus(ctx, out.DocumentID)
	if st.State != domain.IngestAccepted || !strings.Contains(st.Error, "no vector") {
		t.Fatalf("status = %+v, want accepted with the embed failure as its error", st)
	}
}

func TestStagedChunk_RoundTripKeepsShapes(t *testing.T) {
	rel, _ := json.Marshal(map[string]string{"parent_entity_id": "source_doc:x"})
	in := &domain.Document{ID: "d-chunk-1", Text: "body", ActivationStrength: 0.7, CreatedAt: time.Unix(10, 0).UTC(),
		Metadata:   map[string]any{"tags": []string{"a", "b"}, "chunk_relations": json.RawMessage(rel), "index": 3},
		EventStamp: domain.EventStamp{EventTime: time.Unix(5, 0).UTC(), TimeSource: "stated"}}
	staged, err := stageChunks([]*domain.Document{in})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(staged)
	var back []stagedChunk
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	out, err := back[0].document()
	if err != nil {
		t.Fatal(err)
	}
	if tags, ok := out.Metadata["tags"].([]string); !ok || len(tags) != 2 {
		t.Fatalf("tags = %#v, want []string", out.Metadata["tags"])
	}
	if out.ID != in.ID || out.Text != in.Text || out.ActivationStrength != 0.7 || !out.CreatedAt.Equal(in.CreatedAt) ||
		!out.EventStamp.EventTime.Equal(in.EventStamp.EventTime) || out.EventStamp.TimeSource != "stated" {
		t.Fatalf("round trip = %+v", out)
	}
	if out.DocumentType != domain.DocTypeMnemonicFact {
		t.Fatalf("document type = %q", out.DocumentType)
	}
}
