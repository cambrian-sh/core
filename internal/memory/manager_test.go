package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

type fakeVectorStore struct {
	lastOpts domain.SearchOptions
	docs     []domain.SearchResult
	saveErr  error
}

func (f *fakeVectorStore) Save(_ context.Context, _ *domain.Document) error {
	return f.saveErr
}
func (f *fakeVectorStore) GetByID(_ context.Context, _ string) (*domain.Document, error) {
	return nil, nil
}
func (f *fakeVectorStore) GetBatch(_ context.Context, _ []string) ([]domain.Document, error) {
	return nil, nil
}
func (f *fakeVectorStore) SaveBatch(_ context.Context, _ []*domain.Document) error { return nil }
func (f *fakeVectorStore) Search(_ context.Context, _ []float32, opts domain.SearchOptions) ([]domain.SearchResult, error) {
	f.lastOpts = opts
	return f.docs, nil
}
func (f *fakeVectorStore) Delete(_ context.Context, _ string) error          { return nil }
func (f *fakeVectorStore) DeleteBatch(_ context.Context, _ []string) error   { return nil }
func (f *fakeVectorStore) IncrementAccess(_ context.Context, _ string) error { return nil }
func (f *fakeVectorStore) GetStaleMemories(_ context.Context, _ int) ([]domain.Document, error) {
	return nil, nil
}
func (f *fakeVectorStore) QueryByMetadata(_ context.Context, _ map[string]string, _ int) ([]domain.Document, error) {
	return nil, nil
}

type fakeEmbedder struct{}

func (f *fakeEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return []float32{0.1, 0.2, 0.3}, nil
}

// ADR-0025: MemoryManager.Query now queries DocTypeMnemonicFact (DocTypeMemory retired).
func TestMemoryManager_Query_PassesMnemonicFact(t *testing.T) {
	store := &fakeVectorStore{
		docs: []domain.SearchResult{
			{Document: domain.Document{ID: "fact-1", Text: "a fact"}, Score: 0.9},
		},
	}
	mgr := NewMemoryManager(store, &fakeEmbedder{})

	results, err := mgr.Query(context.Background(), "what do I remember?", 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if store.lastOpts.DocumentType != domain.DocTypeMnemonicFact {
		t.Errorf("DocumentType = %q, want %q (DocTypeMemory is retired)",
			store.lastOpts.DocumentType, domain.DocTypeMnemonicFact)
	}
	if store.lastOpts.TopK != 5 {
		t.Errorf("TopK = %d, want 5", store.lastOpts.TopK)
	}
}

func TestVectorStore_Search_EmptyDocumentTypeReturnsAll(t *testing.T) {
	store := &fakeVectorStore{
		docs: []domain.SearchResult{
			{Document: domain.Document{ID: "mem-1"}, Score: 0.9},
			{Document: domain.Document{ID: "prof-1"}, Score: 0.8},
		},
	}
	mgr := NewMemoryManager(store, &fakeEmbedder{})

	vec := []float32{0.1, 0.2, 0.3}
	results, err := mgr.Store.Search(context.Background(), vec, domain.SearchOptions{
		DocumentType: "",
		TopK:         10,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("expected 2 results for empty filter, got %d", len(results))
	}
	if store.lastOpts.DocumentType != "" {
		t.Errorf("expected empty DocumentType to be forwarded as-is, got %q", store.lastOpts.DocumentType)
	}
}

func TestDocTypeConstants_StringValues(t *testing.T) {
	if domain.DocTypeMemory != "memory" {
		t.Errorf("DocTypeMemory = %q, want %q", domain.DocTypeMemory, "memory")
	}
	if domain.DocTypeAgentProfile != "agent_profile" {
		t.Errorf("DocTypeAgentProfile = %q, want %q", domain.DocTypeAgentProfile, "agent_profile")
	}
	if domain.DocTypeJudicialRecord != "judicial_record" {
		t.Errorf("DocTypeJudicialRecord = %q, want %q", domain.DocTypeJudicialRecord, "judicial_record")
	}
}

func TestVectorStore_Interface_SearchAcceptsOptions(_ *testing.T) {
	var _ domain.VectorStore = &fakeVectorStore{}
}

func TestMemoryManager_Query_EmbedError(t *testing.T) {
	errStore := &fakeVectorStore{}
	errMgr := &MemoryManager{
		Store:    errStore,
		Embedder: &alwaysErrEmbedder{err: errors.New("embed failed")},
	}
	_, err := errMgr.Query(context.Background(), "test", 5)
	if err == nil {
		t.Error("expected error from failed embed, got nil")
	}
}

type alwaysErrEmbedder struct {
	err error
}

func (e *alwaysErrEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return nil, e.err
}
