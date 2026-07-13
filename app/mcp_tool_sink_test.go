package app

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// sinkFakeStore is a minimal VectorStore that records saves/deletes by id.
type sinkFakeStore struct{ saved map[string]bool }

func (s *sinkFakeStore) Save(_ context.Context, d *domain.Document) error {
	if s.saved == nil {
		s.saved = map[string]bool{}
	}
	s.saved[d.ID] = true
	return nil
}
func (s *sinkFakeStore) Delete(_ context.Context, id string) error { delete(s.saved, id); return nil }
func (s *sinkFakeStore) SaveBatch(context.Context, []*domain.Document) error { return nil }
func (s *sinkFakeStore) Search(context.Context, []float32, domain.SearchOptions) ([]domain.SearchResult, error) {
	return nil, nil
}
func (s *sinkFakeStore) GetByID(context.Context, string) (*domain.Document, error) { return nil, nil }
func (s *sinkFakeStore) GetBatch(context.Context, []string) ([]domain.Document, error) {
	return nil, nil
}
func (s *sinkFakeStore) DeleteBatch(context.Context, []string) error  { return nil }
func (s *sinkFakeStore) IncrementAccess(context.Context, string) error { return nil }
func (s *sinkFakeStore) GetStaleMemories(context.Context, int) ([]domain.Document, error) {
	return nil, nil
}
func (s *sinkFakeStore) QueryByMetadata(context.Context, map[string]string, int) ([]domain.Document, error) {
	return nil, nil
}

type sinkFakeEmb struct{}

func (sinkFakeEmb) Embed(context.Context, string) ([]float32, error) { return []float32{0.1}, nil }

// On re-sync, the sink registers + indexes newly-advertised tools and removes the
// ones a server no longer advertises (ADR-0043 D8 menu-gating + ADR-0044 re-sync);
// a full drop clears everything for that server.
func TestMCPToolSink_ResyncAddsAndRemoves(t *testing.T) {
	reg := domain.NewInMemoryToolRegistry()
	store := &sinkFakeStore{}
	indexer := &domain.ToolIndexer{Store: store, Embedder: sinkFakeEmb{}}
	sink := newMCPToolSink(reg, indexer)
	ctx := context.Background()

	// Boot: srv advertises a + b (registered + indexed), seeded into the sink.
	for _, n := range []string{"mcp:srv/a", "mcp:srv/b"} {
		tl := domain.SystemTool{Name: n}
		reg.Register(tl)
		_ = indexer.Index(ctx, tl)
	}
	sink.Seed(reg.All())

	// Re-sync: srv now advertises a + c (b dropped, c added).
	sink.SetServerTools(ctx, "srv", []domain.SystemTool{{Name: "mcp:srv/a"}, {Name: "mcp:srv/c"}})

	if _, ok := reg.Get("mcp:srv/b"); ok {
		t.Error("dropped tool b must leave the registry (menu-gating)")
	}
	if _, ok := reg.Get("mcp:srv/c"); !ok {
		t.Error("new tool c must be registered")
	}
	if !store.saved["mcp:srv/c"] {
		t.Error("new tool c must be indexed")
	}
	if store.saved["mcp:srv/b"] {
		t.Error("dropped tool b must be de-indexed")
	}

	// Full drop: all of srv's tools gone from registry + index.
	sink.RemoveServerTools(ctx, "srv")
	if _, ok := reg.Get("mcp:srv/a"); ok {
		t.Error("RemoveServerTools must clear all of the server's tools")
	}
	if store.saved["mcp:srv/a"] {
		t.Error("RemoveServerTools must de-index all of the server's tools")
	}
}
