package chaos_test

import (
	"context"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/testing/chaos"
)

func TestFaultyGenerator_DelegatesBeforeFail(t *testing.T) {
	inner := &stubGenerator{response: "ok"}
	fg := chaos.NewFaultyGenerator(inner, chaos.FaultConfig{AfterSuccesses: 2, Error: chaos.ErrInjected})

	for i := range 2 {
		resp, err := fg.Generate(context.Background(), "prompt")
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if resp != "ok" {
			t.Fatalf("call %d: got %q, want %q", i, resp, "ok")
		}
	}
	if inner.calls != 2 {
		t.Fatalf("inner called %d times, want 2", inner.calls)
	}

	_, err := fg.Generate(context.Background(), "prompt")
	if err == nil || err.Error() != chaos.ErrInjected.Error() {
		t.Fatalf("got %v, want injected error", err)
	}
}

func TestFaultyGenerator_FailsImmediately(t *testing.T) {
	inner := &stubGenerator{}
	fg := chaos.NewFaultyGenerator(inner, chaos.FaultConfig{AfterSuccesses: 0, Error: chaos.ErrInjected})

	_, err := fg.Generate(context.Background(), "prompt")
	if err == nil || err.Error() != chaos.ErrInjected.Error() {
		t.Fatalf("got %v, want injected error", err)
	}
	if inner.calls != 0 {
		t.Fatalf("inner called %d times, want 0", inner.calls)
	}
}

func TestFaultyVectorStore_FailsAfterNSuccesses(t *testing.T) {
	inner := &stubVectorStore{embedding: []float32{1.0}}
	cfg := chaos.FaultConfig{AfterSuccesses: 1, Error: chaos.ErrConnectionRefused}
	fvs := chaos.NewFaultyVectorStore(inner, cfg)

	ctx := context.Background()
	docs, err := fvs.Search(ctx, []float32{1.0}, domain.SearchOptions{TopK: 5})
	if err != nil {
		t.Fatalf("first Search: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("got %d docs, want 1", len(docs))
	}

	_, err = fvs.Search(ctx, []float32{1.0}, domain.SearchOptions{TopK: 5})
	if err == nil || err.Error() != chaos.ErrConnectionRefused.Error() {
		t.Fatalf("got %v, want connection refused", err)
	}
}

func TestFaultyVectorStore_ImplementsInterface(t *testing.T) {
	var _ domain.VectorStore = chaos.NewFaultyVectorStore(nil, chaos.FaultConfig{AfterSuccesses: 0, Error: chaos.ErrDiskFull})
}

func TestFaultyTaskEventWriter_FailsAfterNSuccesses(t *testing.T) {
	inner := &stubTaskEventWriter{}
	cfg := chaos.FaultConfig{AfterSuccesses: 2, Error: chaos.ErrDiskFull}
	fw := chaos.NewFaultyTaskEventWriter(inner, cfg)

	for range 2 {
		if err := fw.WriteTaskEvent(domain.TaskEvent{}); err != nil {
			t.Fatalf("expected success, got: %v", err)
		}
	}

	err := fw.WriteTaskEvent(domain.TaskEvent{})
	if err == nil || err.Error() != chaos.ErrDiskFull.Error() {
		t.Fatalf("got %v, want disk full", err)
	}
	if inner.writes != 2 {
		t.Fatalf("inner writes=%d, want 2", inner.writes)
	}
}

func TestFaultyTaskEventWriter_NilInner(t *testing.T) {
	fw := chaos.NewFaultyTaskEventWriter(nil, chaos.FaultConfig{AfterSuccesses: 0, Error: chaos.ErrDiskFull})
	err := fw.WriteTaskEvent(domain.TaskEvent{})
	if err == nil || err.Error() != chaos.ErrDiskFull.Error() {
		t.Fatalf("got %v, want disk full error", err)
	}
}

type stubGenerator struct {
	calls    int
	response string
}

func (s *stubGenerator) Generate(ctx context.Context, prompt string) (string, error) {
	s.calls++
	if s.response == "" {
		return "ok", nil
	}
	return s.response, nil
}

type stubVectorStore struct {
	embedding []float32
}

func (s *stubVectorStore) Save(ctx context.Context, doc *domain.Document) error    { return nil }
func (s *stubVectorStore) SaveBatch(ctx context.Context, docs []*domain.Document) error { return nil }
func (s *stubVectorStore) Search(ctx context.Context, embedding []float32, opts domain.SearchOptions) ([]domain.SearchResult, error) {
	if s.embedding == nil {
		return nil, nil
	}
	return []domain.SearchResult{{Score: 0.9}}, nil
}
func (s *stubVectorStore) GetByID(ctx context.Context, id string) (*domain.Document, error)         { return nil, nil }
func (s *stubVectorStore) GetBatch(ctx context.Context, ids []string) ([]domain.Document, error)     { return nil, nil }
func (s *stubVectorStore) Delete(ctx context.Context, id string) error                                 { return nil }
func (s *stubVectorStore) DeleteBatch(ctx context.Context, ids []string) error                        { return nil }
func (s *stubVectorStore) IncrementAccess(ctx context.Context, id string) error                       { return nil }
func (s *stubVectorStore) GetStaleMemories(ctx context.Context, limit int) ([]domain.Document, error) { return nil, nil }
func (s *stubVectorStore) QueryByMetadata(_ context.Context, _ map[string]string, _ int) ([]domain.Document, error) {
	return nil, nil
}

type stubTaskEventWriter struct {
	writes int
}

func (w *stubTaskEventWriter) WriteTaskEvent(event domain.TaskEvent) error {
	w.writes++
	return nil
}
