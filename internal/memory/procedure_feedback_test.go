package memory

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// roundTripStore serves docs by id and captures saves — enough to exercise the full
// load → ApplyOutcome → save cycle without a database.
type roundTripStore struct {
	fakeVectorStore
	docs map[string]domain.Document
}

func (r *roundTripStore) GetByID(_ context.Context, id string) (*domain.Document, error) {
	d, ok := r.docs[id]
	if !ok {
		return nil, nil
	}
	return &d, nil
}

func (r *roundTripStore) Save(_ context.Context, doc *domain.Document) error {
	if r.docs == nil {
		r.docs = map[string]domain.Document{}
	}
	r.docs[doc.ID] = *doc
	return nil
}

// The decode must be the exact inverse of the encode. If it is not, every feedback
// cycle silently resets what it does not recover — and sample_count in particular
// round-trips through JSONB as float64, so a naive int assertion would erase the
// corroboration this whole tier is built on.
func TestProcedureDoc_RoundTrips(t *testing.T) {
	orig := domain.Procedure{
		ID:      "proc-1",
		Trigger: "goal: ship a release",
		Steps: []domain.ProcedureStep{
			{RequiredCapabilities: []string{"build"}, Intent: "compile"},
		},
		SourceExperiences: []string{"exp-1", "exp-2"},
		Tags:              []string{"internal"},
		SampleCount:       7,
		Confidence:        0.75,
		Status:            domain.ProcedureActive,
	}
	d := procedureDoc(orig, []float32{0.1})
	// Simulate the JSONB round-trip: numbers come back as float64.
	d.Metadata["sample_count"] = float64(7)
	d.Metadata["confidence"] = float64(0.75)
	d.Metadata["tags"] = []interface{}{"internal"}

	got, ok := procedureFromDoc(*d)
	if !ok {
		t.Fatal("a procedure document must decode")
	}
	if got.SampleCount != 7 {
		t.Errorf("sample count lost across the round trip: %d", got.SampleCount)
	}
	if got.Confidence != 0.75 {
		t.Errorf("confidence lost: %v", got.Confidence)
	}
	if len(got.Steps) != 1 || got.Steps[0].RequiredCapabilities[0] != "build" {
		t.Errorf("steps lost: %+v", got.Steps)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "internal" {
		t.Errorf("tags lost: %+v", got.Tags)
	}
}

// The loop itself: a routine that informed a plan must learn from the outcome.
func TestFeedProcedureOutcome_UpdatesConfidence(t *testing.T) {
	store := &roundTripStore{docs: map[string]domain.Document{}}
	emb := &recordingEmbedder{}
	base := domain.Procedure{
		ID: "proc-1", Trigger: "goal: ship", Status: domain.ProcedureActive,
		Steps: []domain.ProcedureStep{{RequiredCapabilities: []string{"build"}}},
	}
	_ = SaveProcedure(context.Background(), store, emb, base)

	FeedProcedureOutcome(context.Background(), store, emb, []string{"proc-1"}, true, 0.3)
	after, _ := procedureFromDoc(store.docs["proc-1"])
	if after.SampleCount != 1 {
		t.Errorf("an observation must be counted, got %d", after.SampleCount)
	}
	if after.Confidence <= 0 {
		t.Errorf("a success must raise confidence, got %v", after.Confidence)
	}
}

// Sustained failure of a followed routine must eventually retire it — that is the
// loop actually closing rather than merely recording.
func TestFeedProcedureOutcome_SustainedFailureRetires(t *testing.T) {
	store := &roundTripStore{docs: map[string]domain.Document{}}
	emb := &recordingEmbedder{}
	_ = SaveProcedure(context.Background(), store, emb, domain.Procedure{
		ID: "proc-1", Trigger: "goal: ship", Status: domain.ProcedureActive,
		Steps: []domain.ProcedureStep{{RequiredCapabilities: []string{"build"}}},
	})
	for i := 0; i < 30; i++ {
		FeedProcedureOutcome(context.Background(), store, emb, []string{"proc-1"}, false, 0.3)
	}
	final, _ := procedureFromDoc(store.docs["proc-1"])
	if final.Status != domain.ProcedureDeprecated {
		t.Errorf("a routine that keeps failing must retire, got %s (conf %.2f)",
			final.Status, final.Confidence)
	}
}

// Feedback is an enrichment of an enrichment: a missing or unreadable routine must be
// skipped quietly, never surfaced as an error that could fail a plan.
func TestFeedProcedureOutcome_ToleratesMissingRoutines(t *testing.T) {
	store := &roundTripStore{docs: map[string]domain.Document{}}
	FeedProcedureOutcome(context.Background(), store, &recordingEmbedder{},
		[]string{"nope", ""}, true, 0.3) // must not panic
}
