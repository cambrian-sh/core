package domain_test

import (
	"context"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// stubVectorStoreWithMetadata satisfies the full VectorStore interface for testing
// QueryByMetadata behavior on a stub.
type stubVS struct{}

func (s *stubVS) Save(_ context.Context, _ *domain.Document) error          { return nil }
func (s *stubVS) SaveBatch(_ context.Context, _ []*domain.Document) error   { return nil }
func (s *stubVS) Search(_ context.Context, _ []float32, _ domain.SearchOptions) ([]domain.SearchResult, error) {
	return nil, nil
}
func (s *stubVS) GetByID(_ context.Context, _ string) (*domain.Document, error) { return nil, nil }
func (s *stubVS) GetBatch(_ context.Context, _ []string) ([]domain.Document, error) { return nil, nil }
func (s *stubVS) Delete(_ context.Context, _ string) error                        { return nil }
func (s *stubVS) DeleteBatch(_ context.Context, _ []string) error                 { return nil }
func (s *stubVS) IncrementAccess(_ context.Context, _ string) error               { return nil }
func (s *stubVS) GetStaleMemories(_ context.Context, _ int) ([]domain.Document, error) {
	return nil, nil
}
func (s *stubVS) QueryByMetadata(_ context.Context, _ map[string]string, _ int) ([]domain.Document, error) {
	return nil, nil
}

// Cycle 0033-05 — VectorStore interface includes QueryByMetadata.
func TestVectorStore_Interface_HasQueryByMetadata(t *testing.T) {
	// Compile-time assertion: stubVS satisfies domain.VectorStore.
	var _ domain.VectorStore = (*stubVS)(nil)
}
