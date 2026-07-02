package memory

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

type fakeHippEmbedder struct {
	vec []float32
	err error
}

func (f *fakeHippEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return f.vec, f.err
}

type fakeHippStore struct {
	results   []domain.SearchResult
	searchErr error
	saveErr   error
	savedDoc  *domain.Document
	lastOpts  domain.SearchOptions
}

func (f *fakeHippStore) Save(_ context.Context, doc *domain.Document) error {
	f.savedDoc = doc
	return f.saveErr
}
func (f *fakeHippStore) GetByID(_ context.Context, _ string) (*domain.Document, error) {
	return nil, nil
}
func (f *fakeHippStore) GetBatch(_ context.Context, _ []string) ([]domain.Document, error) {
	return nil, nil
}
func (f *fakeHippStore) SaveBatch(_ context.Context, _ []*domain.Document) error { return nil }
func (f *fakeHippStore) Search(_ context.Context, _ []float32, opts domain.SearchOptions) ([]domain.SearchResult, error) {
	f.lastOpts = opts
	return f.results, f.searchErr
}
func (f *fakeHippStore) Delete(_ context.Context, _ string) error          { return nil }
func (f *fakeHippStore) DeleteBatch(_ context.Context, _ []string) error   { return nil }
func (f *fakeHippStore) IncrementAccess(_ context.Context, _ string) error { return nil }
func (f *fakeHippStore) GetStaleMemories(_ context.Context, _ int) ([]domain.Document, error) {
	return nil, nil
}
func (f *fakeHippStore) QueryByMetadata(_ context.Context, _ map[string]string, _ int) ([]domain.Document, error) {
	return nil, nil
}

type captureEmbedder struct {
	captured []string
	vec      []float32
}

func (c *captureEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	c.captured = append(c.captured, text)
	return c.vec, nil
}

func defaultHippEmbedder() *fakeHippEmbedder {
	return &fakeHippEmbedder{vec: []float32{0.1, 0.2, 0.3}}
}

func searchResult(score, confidence float64) domain.SearchResult {
	plan := &domain.ExecutionPlan{Subject: "Music Playback"}
	envelope := ProceduralTemplateV1{Version: 1, Plan: plan}
	b, _ := json.Marshal(envelope)
	return domain.SearchResult{
		Score: score,
		Document: domain.Document{
			Text: string(b),
			Metadata: map[string]interface{}{
				"mean_auction_confidence": confidence,
			},
		},
	}
}

func TestHippocampus_Retrieve_NoResults(t *testing.T) {
	h := NewHippocampus(&fakeHippStore{}, defaultHippEmbedder(), nil)

	plan, sim, conf, err := h.Retrieve(context.Background(), "play KMFDM")

	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if plan != nil {
		t.Errorf("expected nil plan, got %+v", plan)
	}
	if sim != 0 || conf != 0 {
		t.Errorf("expected (0,0), got (%v,%v)", sim, conf)
	}
}

func TestHippocampus_Retrieve_BelowSimilarityThreshold(t *testing.T) {
	store := &fakeHippStore{results: []domain.SearchResult{searchResult(0.80, 0.9)}}
	h := NewHippocampus(store, defaultHippEmbedder(), nil)

	plan, sim, conf, err := h.Retrieve(context.Background(), "play KMFDM")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan != nil || sim != 0 || conf != 0 {
		t.Errorf("expected (nil,0,0), got (%v,%v,%v)", plan, sim, conf)
	}
}

func TestHippocampus_Retrieve_BelowConfidenceFloor(t *testing.T) {
	store := &fakeHippStore{results: []domain.SearchResult{searchResult(0.90, 0.49)}}
	h := NewHippocampus(store, defaultHippEmbedder(), nil)

	plan, sim, conf, err := h.Retrieve(context.Background(), "play KMFDM")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan != nil || sim != 0 || conf != 0 {
		t.Errorf("expected (nil,0,0), got (%v,%v,%v)", plan, sim, conf)
	}
}

func TestHippocampus_Retrieve_ReturnsMatchingPlan(t *testing.T) {
	originalPlan := &domain.ExecutionPlan{Subject: "Music Playback"}
	envelope := ProceduralTemplateV1{Version: 1, Plan: originalPlan}
	marshaledEnvelope, _ := json.Marshal(envelope)

	store := &fakeHippStore{
		results: []domain.SearchResult{
			{
				Score: 0.92,
				Document: domain.Document{
					Text: string(marshaledEnvelope),
					Metadata: map[string]interface{}{
						"mean_auction_confidence": 0.87,
					},
				},
			},
		},
	}

	h := NewHippocampus(store, defaultHippEmbedder(), nil)
	plan, sim, conf, err := h.Retrieve(context.Background(), "play KMFDM")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan == nil {
		t.Fatal("expected non-nil plan")
	}
	if sim != 0.92 {
		t.Errorf("expected similarity 0.92, got %v", sim)
	}
	if conf != 0.87 {
		t.Errorf("expected confidence 0.87, got %v", conf)
	}
}

func TestHippocampus_Retrieve_SearchError_ReturnsInfraError(t *testing.T) {
	store := &fakeHippStore{searchErr: errors.New("pgvector unavailable")}
	h := NewHippocampus(store, defaultHippEmbedder(), nil)

	plan, sim, conf, err := h.Retrieve(context.Background(), "play KMFDM")

	if err == nil {
		t.Error("expected error, got nil")
	}
	if !errors.Is(err, ErrHippocampusFailure) {
		t.Errorf("want ErrHippocampusFailure, got %v", err)
	}
	if plan != nil || sim != 0 || conf != 0 {
		t.Errorf("expected (nil,0,0), got (%v,%v,%v)", plan, sim, conf)
	}
}

func TestHippocampus_Store_EmbedsNormalisedCanonicalKey(t *testing.T) {
	embedder := &captureEmbedder{vec: []float32{0.1}}
	h := NewHippocampus(&fakeHippStore{}, embedder, nil)

	plan := &domain.ExecutionPlan{
		Subject: "Music Playback",
		Steps: []domain.Step{
			{Query: "search for tracks"},
			{Query: "play music"},
		},
	}

	if err := h.Store(context.Background(), plan, 0.88); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(embedder.captured) == 0 {
		t.Fatal("Embed was not called")
	}

	got := embedder.captured[0]
	want := "intent music playback | step search for tracks | step play music"

	if got != want {
		t.Errorf("Got : %q\nWant: %q", got, want)
	}
}

func TestHippocampus_Store_SavesCorrectDocument(t *testing.T) {
	store := &fakeHippStore{}
	h := NewHippocampus(store, &captureEmbedder{vec: []float32{0.1}}, nil)

	plan := &domain.ExecutionPlan{
		Subject: "Music Playback",
		Steps:   []domain.Step{{Query: "play KMFDM"}},
	}

	if err := h.Store(context.Background(), plan, 0.75); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if store.savedDoc == nil {
		t.Fatal("Save was not called")
	}
	if store.savedDoc.DocumentType != domain.DocTypeProceduralTemplate {
		t.Errorf("DocumentType = %q, want %q", store.savedDoc.DocumentType, domain.DocTypeProceduralTemplate)
	}
	conf, ok := store.savedDoc.Metadata["mean_auction_confidence"].(float64)
	if !ok {
		t.Fatalf("mean_auction_confidence missing or wrong type in metadata")
	}
	if conf != 0.75 {
		t.Errorf("mean_auction_confidence = %v, want 0.75", conf)
	}
	if _, ok := store.savedDoc.Metadata["stored_at"]; !ok {
		t.Error("stored_at missing from metadata")
	}
}

func TestHippocampus_Store_SaveError_SilentlyReturnsNil(t *testing.T) {
	store := &fakeHippStore{saveErr: errors.New("db write failed")}
	h := NewHippocampus(store, &captureEmbedder{vec: []float32{0.1}}, nil)

	plan := &domain.ExecutionPlan{Subject: "test", Steps: []domain.Step{}}

	err := h.Store(context.Background(), plan, 0.8)
	if err != nil {
		t.Errorf("expected nil error (silent), got %v", err)
	}
}
