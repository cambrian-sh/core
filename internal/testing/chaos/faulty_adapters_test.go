package chaos_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/testing/chaos"
)

// These tests pin the decorators' own contract (schedule, recovery, delay). They
// are NOT chaos scenarios — those live in scenarios_test.go and wire a decorator
// into a real kernel component.

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
	if !errors.Is(err, chaos.ErrInjected) {
		t.Fatalf("got %v, want injected error", err)
	}
	if fg.Calls() != 3 {
		t.Fatalf("Calls()=%d, want 3", fg.Calls())
	}
}

func TestFaultyGenerator_FailsImmediately(t *testing.T) {
	inner := &stubGenerator{}
	fg := chaos.NewFaultyGenerator(inner, chaos.FaultConfig{AfterSuccesses: 0, Error: chaos.ErrInjected})

	_, err := fg.Generate(context.Background(), "prompt")
	if !errors.Is(err, chaos.ErrInjected) {
		t.Fatalf("got %v, want injected error", err)
	}
	if inner.calls != 0 {
		t.Fatalf("inner called %d times, want 0", inner.calls)
	}
}

// FailCount makes the decorator recover: fail N times, then succeed again.
func TestFaultyGenerator_RecoversAfterFailCount(t *testing.T) {
	fg := chaos.NewFaultyGenerator(nil, chaos.FaultConfig{AfterSuccesses: 1, FailCount: 2, Error: chaos.ErrInjected})
	ctx := context.Background()
	want := []bool{false, true, true, false, false} // ok, fail, fail, ok, ok
	for i, wantErr := range want {
		_, err := fg.Generate(ctx, "p")
		if (err != nil) != wantErr {
			t.Fatalf("call %d: err=%v, wantErr=%v", i, err, wantErr)
		}
	}
}

// Delay stalls the call; a context deadline cuts the stall short and the call
// returns ctx.Err() without reaching the fault decision.
func TestFaultConfig_DelayHonoursContextDeadline(t *testing.T) {
	fg := chaos.NewFaultyGenerator(nil, chaos.FaultConfig{AfterSuccesses: 10, Delay: 10 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := fg.Generate(ctx, "p")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want context.DeadlineExceeded", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("delay ignored the context: took %s", time.Since(start))
	}
	if fg.Calls() != 0 {
		t.Fatalf("a stall cut short must not count as a call, Calls()=%d", fg.Calls())
	}
}

// Delay with no deadline stalls for the full duration, then the schedule applies.
func TestFaultConfig_DelayWithoutDeadlineStalls(t *testing.T) {
	fg := chaos.NewFaultyGenerator(nil, chaos.FaultConfig{AfterSuccesses: 10, Delay: 40 * time.Millisecond})
	start := time.Now()
	if _, err := fg.Generate(context.Background(), "p"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if el := time.Since(start); el < 40*time.Millisecond {
		t.Fatalf("delay not applied: took %s", el)
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
	if !errors.Is(err, chaos.ErrConnectionRefused) {
		t.Fatalf("got %v, want connection refused", err)
	}
}

func TestFaultyVectorStore_ImplementsInterface(t *testing.T) {
	var _ domain.VectorStore = chaos.NewFaultyVectorStore(nil, chaos.FaultConfig{AfterSuccesses: 0, Error: chaos.ErrDiskFull})
}

func TestFaultyEmbedder_BatchAndSingleShareOneSchedule(t *testing.T) {
	fe := chaos.NewFaultyEmbedder(nil, chaos.FaultConfig{AfterSuccesses: 1, Error: chaos.ErrInjected})
	ctx := context.Background()
	vecs, err := fe.EmbedBatch(ctx, []string{"a", "b"})
	if err != nil || len(vecs) != 2 {
		t.Fatalf("EmbedBatch: vecs=%d err=%v", len(vecs), err)
	}
	if _, err := fe.Embed(ctx, "c"); !errors.Is(err, chaos.ErrInjected) {
		t.Fatalf("second call (Embed) should fail, got %v", err)
	}
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
	if !errors.Is(err, chaos.ErrDiskFull) {
		t.Fatalf("got %v, want disk full", err)
	}
	if inner.writes != 2 {
		t.Fatalf("inner writes=%d, want 2", inner.writes)
	}
}

func TestFaultyTaskEventWriter_NilInner(t *testing.T) {
	fw := chaos.NewFaultyTaskEventWriter(nil, chaos.FaultConfig{AfterSuccesses: 0, Error: chaos.ErrDiskFull})
	if err := fw.WriteTaskEvent(domain.TaskEvent{}); !errors.Is(err, chaos.ErrDiskFull) {
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

func (s *stubVectorStore) Save(ctx context.Context, doc *domain.Document) error         { return nil }
func (s *stubVectorStore) SaveBatch(ctx context.Context, docs []*domain.Document) error { return nil }
func (s *stubVectorStore) Search(ctx context.Context, embedding []float32, opts domain.SearchOptions) ([]domain.SearchResult, error) {
	if s.embedding == nil {
		return nil, nil
	}
	return []domain.SearchResult{{Score: 0.9}}, nil
}
func (s *stubVectorStore) GetByID(ctx context.Context, id string) (*domain.Document, error) {
	return nil, nil
}
func (s *stubVectorStore) GetBatch(ctx context.Context, ids []string) ([]domain.Document, error) {
	return nil, nil
}
func (s *stubVectorStore) Delete(ctx context.Context, id string) error          { return nil }
func (s *stubVectorStore) DeleteBatch(ctx context.Context, ids []string) error  { return nil }
func (s *stubVectorStore) IncrementAccess(ctx context.Context, id string) error { return nil }
func (s *stubVectorStore) GetStaleMemories(ctx context.Context, limit int) ([]domain.Document, error) {
	return nil, nil
}
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
