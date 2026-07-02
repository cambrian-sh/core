package memory

import (
	"context"
	"strings"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

type captureSaveStore struct {
	fakeVectorStore
	savedDoc *domain.Document
	saveErr  error
}

func (c *captureSaveStore) Save(_ context.Context, doc *domain.Document) error {
	c.savedDoc = doc
	return c.saveErr
}

func newTestAgent(store domain.VectorStore) *Agent {
	mgr := NewMemoryManager(store, &fakeEmbedder{})
	return NewAgent(mgr, nil, 0.70, 5, 3, 64, 0, 0, 0)
}

func TestFetchContext_FiltersNeuralTrace(t *testing.T) {
	store := &fakeVectorStore{
		docs: []domain.SearchResult{
			{
				Document: domain.Document{
					ID:           "trace-1",
					DocumentType: domain.DocTypeNeuralTrace,
					Text:         "neural trace payload",
				},
				Score: 0.95,
			},
			{
				Document: domain.Document{
					ID:           "mem-1",
					DocumentType: domain.DocTypeMemory,
					Text:         "useful planning memory",
				},
				Score: 0.90,
			},
		},
	}

	agent := newTestAgent(store)
	out := agent.FetchContext(context.Background(), "any query")

	if strings.Contains(out, "neural trace payload") {
		t.Error("FetchContext output must NOT contain neural_trace documents")
	}
	if !strings.Contains(out, "useful planning memory") {
		t.Error("FetchContext output must contain the memory document")
	}
}

func TestFetchContext_NegativeEdge_Included(t *testing.T) {
	store := &fakeVectorStore{
		docs: []domain.SearchResult{
			{
				Document: domain.Document{
					ID:                 "neg-1",
					DocumentType:       domain.DocTypeNegativeEdge,
					Text:               "negative edge failure text",
					ActivationStrength: 0.1,
				},
				Score: 0.85,
			},
		},
	}

	agent := newTestAgent(store)
	out := agent.FetchContext(context.Background(), "any query")

	if !strings.Contains(out, "negative edge failure text") {
		t.Error("FetchContext output must contain negative_edge documents when score > 0.70")
	}
}

func TestIngestNegativeEdge_SavesCorrectDoc(t *testing.T) {
	store := &captureSaveStore{}
	agent := newTestAgent(store)

	err := agent.IngestNegativeEdge(context.Background(), "some error happened", "last output text", "agent-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if store.savedDoc == nil {
		t.Fatal("Store.Save was not called")
	}
	if store.savedDoc.DocumentType != domain.DocTypeNegativeEdge {
		t.Errorf("DocumentType = %q, want %q", store.savedDoc.DocumentType, domain.DocTypeNegativeEdge)
	}
	if store.savedDoc.ActivationStrength != 0.1 {
		t.Errorf("ActivationStrength = %v, want 0.1", store.savedDoc.ActivationStrength)
	}
	if !strings.Contains(store.savedDoc.Text, "some error happened") {
		t.Errorf("Text %q does not contain error message", store.savedDoc.Text)
	}
	if !strings.Contains(store.savedDoc.Text, "last output text") {
		t.Errorf("Text %q does not contain last output", store.savedDoc.Text)
	}
}

func TestIngestNegativeEdge_StoreError_Propagated(t *testing.T) {
	store := &captureSaveStore{saveErr: errStoreFailed}
	agent := newTestAgent(store)

	err := agent.IngestNegativeEdge(context.Background(), "error", "output", "agent-1")
	if err == nil {
		t.Fatal("expected error to be propagated, got nil")
	}
	if err != errStoreFailed {
		t.Errorf("expected errStoreFailed, got %v", err)
	}
}

var errStoreFailed = func() error { return &storeFailedError{} }()

type storeFailedError struct{}

func (e *storeFailedError) Error() string { return "store write failed" }
