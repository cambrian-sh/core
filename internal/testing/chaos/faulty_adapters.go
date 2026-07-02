package chaos

import (
	"context"
	"sync"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

var (
	ErrInjected         = &FaultErr{Msg: "injected fault"}
	ErrConnectionRefused = &FaultErr{Msg: "connection refused"}
	ErrDiskFull         = &FaultErr{Msg: "disk full"}
)

type FaultErr struct{ Msg string }

func (e *FaultErr) Error() string { return e.Msg }

type FaultConfig struct {
	AfterSuccesses int
	Error          error
	Delay          time.Duration
}

type successCounter struct {
	mu        sync.Mutex
	successes int
}

func (c *successCounter) shouldFail(after int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.successes >= after {
		return true
	}
	c.successes++
	return false
}

type FaultyGenerator struct {
	inner    domain.Generator
	cfg      FaultConfig
	counter  successCounter
}

func NewFaultyGenerator(inner domain.Generator, cfg FaultConfig) *FaultyGenerator {
	return &FaultyGenerator{inner: inner, cfg: cfg}
}

func (g *FaultyGenerator) Generate(ctx context.Context, prompt string) (string, error) {
	if g.counter.shouldFail(g.cfg.AfterSuccesses) {
		return "", g.cfg.Error
	}
	if g.inner == nil {
		return "ok", nil
	}
	return g.inner.Generate(ctx, prompt)
}

type FaultyVectorStore struct {
	inner   domain.VectorStore
	cfg     FaultConfig
	counter successCounter
}

func NewFaultyVectorStore(inner domain.VectorStore, cfg FaultConfig) *FaultyVectorStore {
	return &FaultyVectorStore{inner: inner, cfg: cfg}
}

func (s *FaultyVectorStore) Save(ctx context.Context, doc *domain.Document) error {
	if s.counter.shouldFail(s.cfg.AfterSuccesses) {
		return s.cfg.Error
	}
	if s.inner != nil {
		return s.inner.Save(ctx, doc)
	}
	return nil
}

func (s *FaultyVectorStore) SaveBatch(ctx context.Context, docs []*domain.Document) error {
	if s.counter.shouldFail(s.cfg.AfterSuccesses) {
		return s.cfg.Error
	}
	if s.inner != nil {
		return s.inner.SaveBatch(ctx, docs)
	}
	return nil
}

func (s *FaultyVectorStore) Search(ctx context.Context, embedding []float32, opts domain.SearchOptions) ([]domain.SearchResult, error) {
	if s.counter.shouldFail(s.cfg.AfterSuccesses) {
		return nil, s.cfg.Error
	}
	if s.inner != nil {
		return s.inner.Search(ctx, embedding, opts)
	}
	return nil, nil
}

func (s *FaultyVectorStore) GetByID(ctx context.Context, id string) (*domain.Document, error) {
	if s.counter.shouldFail(s.cfg.AfterSuccesses) {
		return nil, s.cfg.Error
	}
	if s.inner != nil {
		return s.inner.GetByID(ctx, id)
	}
	return nil, nil
}

func (s *FaultyVectorStore) GetBatch(ctx context.Context, ids []string) ([]domain.Document, error) {
	if s.counter.shouldFail(s.cfg.AfterSuccesses) {
		return nil, s.cfg.Error
	}
	if s.inner != nil {
		return s.inner.GetBatch(ctx, ids)
	}
	return nil, nil
}

func (s *FaultyVectorStore) Delete(ctx context.Context, id string) error {
	if s.counter.shouldFail(s.cfg.AfterSuccesses) {
		return s.cfg.Error
	}
	if s.inner != nil {
		return s.inner.Delete(ctx, id)
	}
	return nil
}

func (s *FaultyVectorStore) DeleteBatch(ctx context.Context, ids []string) error {
	if s.counter.shouldFail(s.cfg.AfterSuccesses) {
		return s.cfg.Error
	}
	if s.inner != nil {
		return s.inner.DeleteBatch(ctx, ids)
	}
	return nil
}

func (s *FaultyVectorStore) IncrementAccess(ctx context.Context, id string) error {
	if s.counter.shouldFail(s.cfg.AfterSuccesses) {
		return s.cfg.Error
	}
	if s.inner != nil {
		return s.inner.IncrementAccess(ctx, id)
	}
	return nil
}

func (s *FaultyVectorStore) GetStaleMemories(ctx context.Context, limit int) ([]domain.Document, error) {
	if s.counter.shouldFail(s.cfg.AfterSuccesses) {
		return nil, s.cfg.Error
	}
	if s.inner != nil {
		return s.inner.GetStaleMemories(ctx, limit)
	}
	return nil, nil
}

func (s *FaultyVectorStore) QueryByMetadata(ctx context.Context, filter map[string]string, limit int) ([]domain.Document, error) {
	if s.inner != nil {
		return s.inner.QueryByMetadata(ctx, filter, limit)
	}
	return nil, nil
}

type FaultyTaskEventWriter struct {
	inner   taskEventWriter
	cfg     FaultConfig
	counter successCounter
}

type taskEventWriter interface {
	WriteTaskEvent(event domain.TaskEvent) error
}

func NewFaultyTaskEventWriter(inner taskEventWriter, cfg FaultConfig) *FaultyTaskEventWriter {
	return &FaultyTaskEventWriter{inner: inner, cfg: cfg}
}

func (w *FaultyTaskEventWriter) WriteTaskEvent(event domain.TaskEvent) error {
	if w.counter.shouldFail(w.cfg.AfterSuccesses) {
		return w.cfg.Error
	}
	if w.inner != nil {
		return w.inner.WriteTaskEvent(event)
	}
	return nil
}

var (
	_ domain.VectorStore = (*FaultyVectorStore)(nil)
	_ domain.Generator   = (*FaultyGenerator)(nil)
)
