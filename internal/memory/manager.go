package memory

import (
	"context"
	"fmt"

	"github.com/cambrian-sh/core/domain"
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

// Save persists an ALREADY-EMBEDDED document.
//
// Distinct from Ingest, which embeds doc.Text first: callers that have computed a
// vector themselves (batched embedding, a procedure trigger, an action line) must
// not pay for a second embed, and passing them through
// Ingest would silently overwrite the vector they built.
//
// It exists so that writing a document is a method call on the manager rather than
// a reach through its Store field. Eleven call sites in this package went straight
// to the embedded store instead, which meant the manager was not the write path —
// it was one of several, and the public field was the real interface.
// Anything that must hold for every write in this package (instrumentation, a
// metrics counter, a doc-type assertion) now has exactly one place to live.
func (m *MemoryManager) Save(ctx context.Context, doc *domain.Document) error {
	return m.Store.Save(ctx, doc)
}

// SaveBatch is the batched form of Save; same reasoning.
func (m *MemoryManager) SaveBatch(ctx context.Context, docs []*domain.Document) error {
	return m.Store.SaveBatch(ctx, docs)
}

// GetByID / GetBatch read through the enforcing store under the CALLER's scope, as
// it travels on ctx (AGENTS.md §2.4). They used to overwrite whatever predicate the
// caller carried with domain.ScopeSystem, on the claim that only kernel-internal code
// called them — a principal path reaching them would have read every principal's rows
// (owner decision 2026-11-15, ENG-540). A caller with no principal states that
// explicitly with domain.WithScope(ctx, domain.ScopeSystem) (greppable); a ctx with no
// predicate fails closed (authz.ErrScopeMissing), by design.
func (m *MemoryManager) GetByID(ctx context.Context, id string) (*domain.Document, error) {
	return m.Store.GetByID(ctx, id)
}

func (m *MemoryManager) GetBatch(ctx context.Context, ids []string) ([]domain.Document, error) {
	return m.Store.GetBatch(ctx, ids)
}

func (m *MemoryManager) IncrementAccess(ctx context.Context, id string) error {
	return m.Store.IncrementAccess(ctx, id)
}
