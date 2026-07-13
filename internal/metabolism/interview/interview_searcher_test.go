package interview

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// fakeSearchVectorStore is a minimal VectorStore stub for InterviewSearcher tests.
type fakeSearchVectorStore struct {
	results []domain.SearchResult
}

func (f *fakeSearchVectorStore) Save(_ context.Context, _ *domain.Document) error { return nil }
func (f *fakeSearchVectorStore) SaveBatch(_ context.Context, _ []*domain.Document) error {
	return nil
}
func (f *fakeSearchVectorStore) Search(_ context.Context, _ []float32, _ domain.SearchOptions) ([]domain.SearchResult, error) {
	return f.results, nil
}
func (f *fakeSearchVectorStore) GetByID(_ context.Context, _ string) (*domain.Document, error) {
	return nil, nil
}
func (f *fakeSearchVectorStore) GetBatch(_ context.Context, _ []string) ([]domain.Document, error) {
	return nil, nil
}
func (f *fakeSearchVectorStore) Delete(_ context.Context, _ string) error          { return nil }
func (f *fakeSearchVectorStore) DeleteBatch(_ context.Context, _ []string) error   { return nil }
func (f *fakeSearchVectorStore) IncrementAccess(_ context.Context, _ string) error { return nil }
func (f *fakeSearchVectorStore) GetStaleMemories(_ context.Context, _ int) ([]domain.Document, error) {
	return nil, nil
}
func (f *fakeSearchVectorStore) QueryByMetadata(_ context.Context, _ map[string]string, _ int) ([]domain.Document, error) {
	return nil, nil
}

// TestSearchByEmbedding_FiltersToAgentProfile verifies that only documents with
// DocumentType == DocTypeAgentProfile are returned; memory docs are excluded.
func TestSearchByEmbedding_FiltersToAgentProfile(t *testing.T) {
	store := &fakeSearchVectorStore{
		results: []domain.SearchResult{
			{
				Document: domain.Document{
					ID:           "profile-1",
					DocumentType: domain.DocTypeAgentProfile,
					Metadata:     map[string]interface{}{"agent_id": "agent-a", "source_hash": "hash-a"},
				},
				Score: 0.9,
			},
			{
				Document: domain.Document{
					ID:           "mem-1",
					DocumentType: domain.DocTypeMemory,
					Metadata:     map[string]interface{}{},
				},
				Score: 0.85,
			},
		},
	}

	searcher := NewInterviewSearcher(store)
	results, err := searcher.SearchByEmbedding(context.Background(), []float32{0.1, 0.2}, 0.5, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result (agent_profile only), got %d", len(results))
	}
	if results[0].Similarity != 0.9 {
		t.Errorf("expected Similarity=0.9, got %f", results[0].Similarity)
	}
}

// TestSearchByEmbedding_ThresholdFilter verifies that results with Score below
// the threshold are excluded even if they have the correct DocumentType.
func TestSearchByEmbedding_ThresholdFilter(t *testing.T) {
	store := &fakeSearchVectorStore{
		results: []domain.SearchResult{
			{
				Document: domain.Document{
					ID:           "profile-low",
					DocumentType: domain.DocTypeAgentProfile,
					Metadata:     map[string]interface{}{"agent_id": "agent-b", "source_hash": "hash-b"},
				},
				Score: 0.6, // below threshold of 0.8
			},
		},
	}

	searcher := NewInterviewSearcher(store)
	results, err := searcher.SearchByEmbedding(context.Background(), []float32{0.1, 0.2}, 0.8, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results (score below threshold), got %d", len(results))
	}
}

// TestSearchByEmbedding_OrderedDescending verifies that results are returned
// ordered by Similarity descending (highest similarity first).
func TestSearchByEmbedding_OrderedDescending(t *testing.T) {
	store := &fakeSearchVectorStore{
		results: []domain.SearchResult{
			{
				Document: domain.Document{
					ID:           "profile-low",
					DocumentType: domain.DocTypeAgentProfile,
					Metadata:     map[string]interface{}{"agent_id": "agent-c", "source_hash": "hash-c"},
				},
				Score: 0.7,
			},
			{
				Document: domain.Document{
					ID:           "profile-high",
					DocumentType: domain.DocTypeAgentProfile,
					Metadata:     map[string]interface{}{"agent_id": "agent-d", "source_hash": "hash-d"},
				},
				Score: 0.9,
			},
		},
	}

	searcher := NewInterviewSearcher(store)
	results, err := searcher.SearchByEmbedding(context.Background(), []float32{0.1, 0.2}, 0.5, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Similarity != 0.9 {
		t.Errorf("expected results[0].Similarity=0.9 (highest first), got %f", results[0].Similarity)
	}
	if results[1].Similarity != 0.7 {
		t.Errorf("expected results[1].Similarity=0.7, got %f", results[1].Similarity)
	}
}

// TestFakeInterviewSearcher_ReturnsConfiguredResults verifies that
// fakeInterviewSearcher returns the configured agentID→similarity map, filtered
// by threshold.
func TestFakeInterviewSearcher_ReturnsConfiguredResults(t *testing.T) {
	fake := &fakeInterviewSearcher{
		results: map[string]float64{
			"agent-x": 0.92,
			"agent-y": 0.45, // below threshold 0.5
		},
	}

	out, err := fake.SearchByEmbedding(context.Background(), []float32{0.1}, 0.5, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 result (agent-y below threshold), got %d", len(out))
	}
	if out[0].AgentID != "agent-x" {
		t.Errorf("expected AgentID=agent-x, got %q", out[0].AgentID)
	}
	if out[0].Similarity != 0.92 {
		t.Errorf("expected Similarity=0.92, got %f", out[0].Similarity)
	}
}
