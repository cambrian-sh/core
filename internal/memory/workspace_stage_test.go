package memory

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// mockFactStore implements VectorStore for workspace tests.
type mockFactStore struct {
	fakeVectorStore
	docs    []domain.SearchResult
	negDocs []domain.SearchResult // returned for DocTypeNegativeEdge queries
}

func (m *mockFactStore) Search(_ context.Context, _ []float32, opts domain.SearchOptions) ([]domain.SearchResult, error) {
	if opts.DocumentType == domain.DocTypeNegativeEdge {
		return m.negDocs, nil
	}
	return m.docs, nil
}

func (m *mockFactStore) GetBatch(_ context.Context, _ []string) ([]domain.Document, error) {
	return nil, nil
}

func factDocs(n int) []domain.SearchResult {
	docs := make([]domain.SearchResult, n)
	for i := range docs {
		docs[i] = domain.SearchResult{
			Document: domain.Document{
				ID:           fmt.Sprintf("fact-%d", i),
				Text:         fmt.Sprintf("fact %d", i),
				DocumentType: domain.DocTypeMnemonicFact,
				Metadata:     map[string]interface{}{"source_agent": "test_agent"},
			},
			Score: float64(n-i) * 0.1,
		}
	}
	return docs
}

// Cycle 1: Facts returned in LTMEnrichment.Facts.
func TestWorkspaceStage_LTMEnrichmentFacts(t *testing.T) {
	store := &mockFactStore{docs: factDocs(2)}
	ws := NewWorkspaceStage(store, &fakeEmbedder{}, nil, 10, 5, 0.2, false, 0.7)

	result, err := ws.PrimeForPlanning(context.Background(), "auth deployment")
	if err != nil {
		t.Fatalf("PrimeForPlanning failed: %v", err)
	}
	if len(result.Facts) != 2 {
		t.Fatalf("expected 2 facts, got %d", len(result.Facts))
	}
}

// Cycle 2: NegativeEdge docs appear in LTMEnrichment.Negatives, not Facts.
func TestWorkspaceStage_NegativesInEnrichment(t *testing.T) {
	store := &mockFactStore{
		docs: factDocs(1),
		negDocs: []domain.SearchResult{
			{Document: domain.Document{
				ID:           "neg-1",
				Text:         "BLOCKED: 'rm' is not in ALLOWED_COMMANDS",
				DocumentType: domain.DocTypeNegativeEdge,
				Metadata:     map[string]interface{}{"agent_id": "terminal_agent"},
			}},
		},
	}
	ws := NewWorkspaceStage(store, &fakeEmbedder{}, nil, 10, 5, 0.2, false, 0.7)

	result, err := ws.PrimeForPlanning(context.Background(), "some query")
	if err != nil {
		t.Fatalf("PrimeForPlanning failed: %v", err)
	}
	if len(result.Negatives) != 1 {
		t.Fatalf("expected 1 negative, got %d", len(result.Negatives))
	}
	for _, f := range result.Facts {
		if f.Document.DocumentType == domain.DocTypeNegativeEdge {
			t.Error("negative_edge doc must NOT appear in Facts")
		}
	}
}

// negLaneStore captures the vector and options the negatives lane searched with. The
// shared mockFactStore discards both, which is how the lane searched with a nil vector
// for as long as it did.
type negLaneStore struct {
	fakeVectorStore
	mu      sync.Mutex
	negVec  []float32
	negOpts domain.SearchOptions
	seenNeg bool
}

func (n *negLaneStore) Search(_ context.Context, vec []float32, opts domain.SearchOptions) ([]domain.SearchResult, error) {
	if opts.DocumentType == domain.DocTypeNegativeEdge {
		n.mu.Lock()
		n.negVec, n.negOpts, n.seenNeg = vec, opts, true
		n.mu.Unlock()
	}
	return nil, nil
}

func (n *negLaneStore) GetBatch(_ context.Context, _ []string) ([]domain.Document, error) {
	return nil, nil
}

// A nil query vector is not similarity-matched to anything: the pgvector adapter answers
// it with an identical score per row ordered by last_accessed_at, so the negatives lane
// fed the planner the three most recently touched failures whatever the task was.
func TestWorkspaceStage_NegativesSearchedAgainstTheTask(t *testing.T) {
	store := &negLaneStore{}
	ws := NewWorkspaceStage(store, &fakeEmbedder{}, nil, 10, 5, 0.35, false, 0.7)

	if _, err := ws.PrimeForPlanning(context.Background(), "auth deployment"); err != nil {
		t.Fatalf("PrimeForPlanning failed: %v", err)
	}
	if !store.seenNeg {
		t.Fatal("the negatives lane never ran")
	}
	want, err := (&fakeEmbedder{}).Embed(context.Background(), "auth deployment")
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(store.negVec) != len(want) {
		t.Fatalf("negatives searched with a %d-dim vector, want the %d-dim task embedding",
			len(store.negVec), len(want))
	}
	for i := range want {
		if store.negVec[i] != want[i] {
			t.Fatalf("negatives vector != embedded task query: got %v want %v", store.negVec, want)
		}
	}
	if store.negOpts.RetrievalFloor != ws.RetrievalFloor {
		t.Errorf("RetrievalFloor: want %v got %v", ws.RetrievalFloor, store.negOpts.RetrievalFloor)
	}
}

// Cycle 3: PrimeForExecution slot limit enforced (map format for DAGExecutor).
func TestWorkspaceStage_SlotLimit(t *testing.T) {
	store := &mockFactStore{docs: factDocs(8)}
	ws := NewWorkspaceStage(store, &fakeEmbedder{}, nil, 10, 3, 0.2, false, 0.7)

	result, err := ws.PrimeForExecution(context.Background(), &domain.ExecutionPlan{Subject: "test"}, nil)
	if err != nil {
		t.Fatalf("PrimeForExecution failed: %v", err)
	}
	if len(result) != 3 {
		t.Errorf("got %d entries, want exactly 3 (slot limit)", len(result))
	}
}

// Cycle 4: empty FACT results → empty LTMEnrichment, no panic.
func TestWorkspaceStage_EmptyResults(t *testing.T) {
	store := &mockFactStore{docs: nil}
	ws := NewWorkspaceStage(store, &fakeEmbedder{}, nil, 10, 5, 0.2, false, 0.7)

	result, err := ws.PrimeForPlanning(context.Background(), "unknown query")
	if err != nil {
		t.Fatalf("PrimeForPlanning failed: %v", err)
	}
	if len(result.Facts) != 0 || len(result.Negatives) != 0 {
		t.Errorf("expected empty enrichment, got facts=%d negatives=%d", len(result.Facts), len(result.Negatives))
	}
}

// Cycle 5: execution slots used for PrimeForExecution.
func TestWorkspaceStage_ExecutionSlots(t *testing.T) {
	store := &mockFactStore{docs: factDocs(10)}
	ws := NewWorkspaceStage(store, &fakeEmbedder{}, nil, 10, 5, 0.2, false, 0.7)

	result, err := ws.PrimeForExecution(context.Background(), &domain.ExecutionPlan{Subject: "test"}, nil)
	if err != nil {
		t.Fatalf("PrimeForExecution failed: %v", err)
	}
	if len(result) != 5 {
		t.Errorf("got %d entries, want exactly 5 (execution slots)", len(result))
	}
}
