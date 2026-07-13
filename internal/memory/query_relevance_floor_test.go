package memory

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// scoredStore returns each doc with a caller-set cosine Score, so the relevance
// floor (ADR-0048 #1) can be exercised deterministically.
type scoredStore struct{ results []domain.SearchResult }

func (s *scoredStore) Search(context.Context, []float32, domain.SearchOptions) ([]domain.SearchResult, error) {
	return s.results, nil
}
func (s *scoredStore) Save(context.Context, *domain.Document) error        { return nil }
func (s *scoredStore) SaveBatch(context.Context, []*domain.Document) error { return nil }
func (s *scoredStore) GetByID(context.Context, string) (*domain.Document, error) {
	return nil, nil
}
func (s *scoredStore) GetBatch(context.Context, []string) ([]domain.Document, error) {
	return nil, nil
}
func (s *scoredStore) Delete(context.Context, string) error        { return nil }
func (s *scoredStore) DeleteBatch(context.Context, []string) error { return nil }
func (s *scoredStore) IncrementAccess(context.Context, string) error {
	return nil
}
func (s *scoredStore) GetStaleMemories(context.Context, int) ([]domain.Document, error) {
	return nil, nil
}
func (s *scoredStore) QueryByMetadata(context.Context, map[string]string, int) ([]domain.Document, error) {
	return nil, nil
}

func fact(id string, score float64) domain.SearchResult {
	return domain.SearchResult{Document: domain.Document{ID: id, Text: id}, Score: score}
}

// Below-floor seeds are dropped; at/above-floor seeds are kept.
func TestQuerySearch_RelevanceFloorDropsIrrelevant(t *testing.T) {
	store := &scoredStore{results: []domain.SearchResult{
		fact("relevant", 0.80),
		fact("borderline", 0.25), // == floor → kept (not below)
		fact("river-junk", 0.10), // unrelated promoted tool output → DROP
	}}
	qs := NewQueryService(&fakeEmbedder{}, store)
	qs.SetRelevanceFloor(0.25)

	got, err := qs.Search(context.Background(), "q", "agent")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	ids := map[string]bool{}
	for _, r := range got {
		ids[r.Document.ID] = true
	}
	if !ids["relevant"] || !ids["borderline"] {
		t.Errorf("at/above-floor seeds must be kept; got %v", ids)
	}
	if ids["river-junk"] {
		t.Error("below-floor seed must be dropped as irrelevant")
	}
}

// An all-irrelevant query returns EMPTY (the signal the agent reads as "no relevant
// memory"), not a padded top-k of junk.
func TestQuerySearch_RelevanceFloorEmptyWhenAllBelow(t *testing.T) {
	store := &scoredStore{results: []domain.SearchResult{
		fact("junk-a", 0.12),
		fact("junk-b", 0.08),
	}}
	qs := NewQueryService(&fakeEmbedder{}, store)
	qs.SetRelevanceFloor(0.25)

	got, err := qs.Search(context.Background(), "q", "agent")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("all-below-floor query must return empty; got %d results", len(got))
	}
}

// Floor 0 disables the gate (legacy flat top-k): nothing is dropped on score.
func TestQuerySearch_RelevanceFloorZeroDisabled(t *testing.T) {
	store := &scoredStore{results: []domain.SearchResult{
		fact("a", 0.9),
		fact("b", 0.01),
	}}
	qs := NewQueryService(&fakeEmbedder{}, store)
	// no SetRelevanceFloor → floor 0

	got, err := qs.Search(context.Background(), "q", "agent")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("floor 0 must keep all results; got %d", len(got))
	}
}
