package memory

import (
	"context"
	"fmt"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// MemoryManager coordinates text digestion and semantic retrieval.
// It bridges the Embedder and the VectorStore.
type MemoryManager struct {
	Store    domain.VectorStore
	Embedder domain.Embedder
}

func NewMemoryManager(store domain.VectorStore, embedder domain.Embedder) *MemoryManager {
	return &MemoryManager{
		Store:    store,
		Embedder: embedder,
	}
}

// Ingest converts a single document into its embedding and stores it in the DB.
func (m *MemoryManager) Ingest(ctx context.Context, doc *domain.Document) error {
	vector, err := m.Embedder.Embed(ctx, doc.Text)
	if err != nil {
		return fmt.Errorf("failed to embed document text: %w", err)
	}

	doc.Embedding = domain.Embedding{
		Vector: vector,
		Model:  "dynamic",
		Size:   len(vector),
	}

	return m.Store.Save(ctx, doc)
}

// Query performs a semantic search for memory documents only.
func (m *MemoryManager) Query(ctx context.Context, prompt string, topK int) ([]domain.SearchResult, error) {
	queryVector, err := m.Embedder.Embed(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("failed to embed query: %w", err)
	}

	return m.Store.Search(ctx, queryVector, domain.SearchOptions{
		DocumentType: domain.DocTypeMnemonicFact,
		TopK:         topK,
		Scope:        domain.ScopeSystem, // ADR-0034: kernel-internal read (not the agent RPC path)
	})
}

func (m *MemoryManager) GetByID(ctx context.Context, id string) (*domain.Document, error) {
	return m.Store.GetByID(ctx, id)
}

func (m *MemoryManager) GetBatch(ctx context.Context, ids []string) ([]domain.Document, error) {
	return m.Store.GetBatch(ctx, ids)
}

func (m *MemoryManager) IncrementAccess(ctx context.Context, id string) error {
	return m.Store.IncrementAccess(ctx, id)
}
