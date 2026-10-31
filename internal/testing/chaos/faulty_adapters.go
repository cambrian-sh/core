// Package chaos provides fault-injecting decorators for the kernel's driven
// ports. Each Faulty* type wraps an optional inner implementation and applies
// one FaultConfig: an optional stall (Delay), then a scripted failure schedule.
//
// The decorators are the INPUT side of a chaos scenario only. A scenario is
// meaningful when the decorator is wired into the real consumer (Gatekeeper,
// LLM Provider, IngestionManager, DAGExecutor, …) and the assertion is on the
// consumer's recovery contract — see scenarios_test.go. Asserting on the
// decorator itself proves nothing about the kernel (the 2026-10-31 audit found
// exactly that: T-20261031-per-pr-chaos-gate-is-vacuous-the-four-scenarios).
package chaos

import (
	"context"
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
)

var (
	ErrInjected          = &FaultErr{Msg: "injected fault"}
	ErrConnectionRefused = &FaultErr{Msg: "connection refused"}
	ErrDiskFull          = &FaultErr{Msg: "disk full"}
)

type FaultErr struct{ Msg string }

func (e *FaultErr) Error() string { return e.Msg }

// FaultConfig scripts one decorator.
//
//	AfterSuccesses  calls that succeed before the fault trips (0 ⇒ trips at once)
//	FailCount       failures returned once tripped; 0 ⇒ fail forever, N ⇒ the
//	                decorator recovers (succeeds again) after N failures — the
//	                shape a circuit breaker's half-open probe needs to see
//	Error           the error returned while tripped
//	Delay           stall applied to EVERY call before the fault decision. With a
//	                ctx the stall is cut short by ctx.Done() and the call returns
//	                ctx.Err(); a ctx-less method sleeps the full Delay.
type FaultConfig struct {
	AfterSuccesses int
	FailCount      int
	Error          error
	Delay          time.Duration
}

// wait applies cfg.Delay. ctx may be nil for methods that carry no context.
func (cfg FaultConfig) wait(ctx context.Context) error {
	if cfg.Delay <= 0 {
		return nil
	}
	if ctx == nil {
		time.Sleep(cfg.Delay)
		return nil
	}
	t := time.NewTimer(cfg.Delay)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// faultSchedule is the shared call counter behind every decorator.
type faultSchedule struct {
	mu        sync.Mutex
	calls     int
	successes int
	failures  int
}

// shouldFail records one call and reports whether it is scripted to fail.
func (s *faultSchedule) shouldFail(cfg FaultConfig) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.successes < cfg.AfterSuccesses {
		s.successes++
		return false
	}
	if cfg.FailCount > 0 && s.failures >= cfg.FailCount {
		return false // recovered
	}
	s.failures++
	return true
}

func (s *faultSchedule) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// gate applies the delay then the failure schedule; a non-nil error means the
// call must return it without delegating.
func gate(ctx context.Context, cfg FaultConfig, s *faultSchedule) error {
	if err := cfg.wait(ctx); err != nil {
		return err
	}
	if s.shouldFail(cfg) {
		return cfg.Error
	}
	return nil
}

// ---------------------------------------------------------------- Generator

type FaultyGenerator struct {
	inner domain.Generator
	cfg   FaultConfig
	sched faultSchedule
}

func NewFaultyGenerator(inner domain.Generator, cfg FaultConfig) *FaultyGenerator {
	return &FaultyGenerator{inner: inner, cfg: cfg}
}

// Calls is the number of Generate calls that reached the fault decision
// (a stall cut short by ctx does not count: the schedule never saw it).
func (g *FaultyGenerator) Calls() int { return g.sched.count() }

func (g *FaultyGenerator) Generate(ctx context.Context, prompt string) (string, error) {
	if err := gate(ctx, g.cfg, &g.sched); err != nil {
		return "", err
	}
	if g.inner == nil {
		return "ok", nil
	}
	return g.inner.Generate(ctx, prompt)
}

// ---------------------------------------------------------------- Embedder

// FaultyEmbedder faults both the single and the batch embed paths with one
// schedule, so a consumer's batch→per-item fallback is exercised by one config.
// With a nil inner it returns a fixed 3-dim vector.
type FaultyEmbedder struct {
	inner domain.Embedder
	cfg   FaultConfig
	sched faultSchedule
}

func NewFaultyEmbedder(inner domain.Embedder, cfg FaultConfig) *FaultyEmbedder {
	return &FaultyEmbedder{inner: inner, cfg: cfg}
}

func (e *FaultyEmbedder) Calls() int { return e.sched.count() }

func (e *FaultyEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if err := gate(ctx, e.cfg, &e.sched); err != nil {
		return nil, err
	}
	if e.inner == nil {
		return []float32{0.1, 0.2, 0.3}, nil
	}
	return e.inner.Embed(ctx, text)
}

func (e *FaultyEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if err := gate(ctx, e.cfg, &e.sched); err != nil {
		return nil, err
	}
	if b, ok := e.inner.(domain.BatchEmbedder); ok {
		return b.EmbedBatch(ctx, texts)
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, err := e.innerEmbed(ctx, t)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func (e *FaultyEmbedder) innerEmbed(ctx context.Context, text string) ([]float32, error) {
	if e.inner == nil {
		return []float32{0.1, 0.2, 0.3}, nil
	}
	return e.inner.Embed(ctx, text)
}

// ---------------------------------------------------------------- InterviewSearcher

// FaultyInterviewSearcher faults the Gatekeeper's Layer-2 semantic search (the
// pgvector-backed interview index). With a nil inner it returns no matches.
type FaultyInterviewSearcher struct {
	inner domain.InterviewSearcher
	cfg   FaultConfig
	sched faultSchedule
}

func NewFaultyInterviewSearcher(inner domain.InterviewSearcher, cfg FaultConfig) *FaultyInterviewSearcher {
	return &FaultyInterviewSearcher{inner: inner, cfg: cfg}
}

func (s *FaultyInterviewSearcher) Calls() int { return s.sched.count() }

func (s *FaultyInterviewSearcher) SearchByEmbedding(ctx context.Context, embedding []float32, threshold float64, topK int) ([]domain.AgentSearchResult, error) {
	if err := gate(ctx, s.cfg, &s.sched); err != nil {
		return nil, err
	}
	if s.inner == nil {
		return nil, nil
	}
	return s.inner.SearchByEmbedding(ctx, embedding, threshold, topK)
}

// ---------------------------------------------------------------- VectorStore

type FaultyVectorStore struct {
	inner domain.VectorStore
	cfg   FaultConfig
	sched faultSchedule
}

func NewFaultyVectorStore(inner domain.VectorStore, cfg FaultConfig) *FaultyVectorStore {
	return &FaultyVectorStore{inner: inner, cfg: cfg}
}

func (s *FaultyVectorStore) Calls() int { return s.sched.count() }

func (s *FaultyVectorStore) Save(ctx context.Context, doc *domain.Document) error {
	if err := gate(ctx, s.cfg, &s.sched); err != nil {
		return err
	}
	if s.inner != nil {
		return s.inner.Save(ctx, doc)
	}
	return nil
}

func (s *FaultyVectorStore) SaveBatch(ctx context.Context, docs []*domain.Document) error {
	if err := gate(ctx, s.cfg, &s.sched); err != nil {
		return err
	}
	if s.inner != nil {
		return s.inner.SaveBatch(ctx, docs)
	}
	return nil
}

func (s *FaultyVectorStore) Search(ctx context.Context, embedding []float32, opts domain.SearchOptions) ([]domain.SearchResult, error) {
	if err := gate(ctx, s.cfg, &s.sched); err != nil {
		return nil, err
	}
	if s.inner != nil {
		return s.inner.Search(ctx, embedding, opts)
	}
	return nil, nil
}

func (s *FaultyVectorStore) GetByID(ctx context.Context, id string) (*domain.Document, error) {
	if err := gate(ctx, s.cfg, &s.sched); err != nil {
		return nil, err
	}
	if s.inner != nil {
		return s.inner.GetByID(ctx, id)
	}
	return nil, nil
}

func (s *FaultyVectorStore) GetBatch(ctx context.Context, ids []string) ([]domain.Document, error) {
	if err := gate(ctx, s.cfg, &s.sched); err != nil {
		return nil, err
	}
	if s.inner != nil {
		return s.inner.GetBatch(ctx, ids)
	}
	return nil, nil
}

func (s *FaultyVectorStore) Delete(ctx context.Context, id string) error {
	if err := gate(ctx, s.cfg, &s.sched); err != nil {
		return err
	}
	if s.inner != nil {
		return s.inner.Delete(ctx, id)
	}
	return nil
}

func (s *FaultyVectorStore) DeleteBatch(ctx context.Context, ids []string) error {
	if err := gate(ctx, s.cfg, &s.sched); err != nil {
		return err
	}
	if s.inner != nil {
		return s.inner.DeleteBatch(ctx, ids)
	}
	return nil
}

func (s *FaultyVectorStore) IncrementAccess(ctx context.Context, id string) error {
	if err := gate(ctx, s.cfg, &s.sched); err != nil {
		return err
	}
	if s.inner != nil {
		return s.inner.IncrementAccess(ctx, id)
	}
	return nil
}

func (s *FaultyVectorStore) GetStaleMemories(ctx context.Context, limit int) ([]domain.Document, error) {
	if err := gate(ctx, s.cfg, &s.sched); err != nil {
		return nil, err
	}
	if s.inner != nil {
		return s.inner.GetStaleMemories(ctx, limit)
	}
	return nil, nil
}

func (s *FaultyVectorStore) QueryByMetadata(ctx context.Context, filter map[string]string, limit int) ([]domain.Document, error) {
	if err := gate(ctx, s.cfg, &s.sched); err != nil {
		return nil, err
	}
	if s.inner != nil {
		return s.inner.QueryByMetadata(ctx, filter, limit)
	}
	return nil, nil
}

// ---------------------------------------------------------------- TaskEventWriter

// taskEventWriter mirrors executer.TaskEventWriter (the bbolt adapter's
// WriteTaskEvent) without importing the executer package.
type taskEventWriter interface {
	WriteTaskEvent(event domain.TaskEvent) error
}

type FaultyTaskEventWriter struct {
	inner taskEventWriter
	cfg   FaultConfig
	sched faultSchedule
}

func NewFaultyTaskEventWriter(inner taskEventWriter, cfg FaultConfig) *FaultyTaskEventWriter {
	return &FaultyTaskEventWriter{inner: inner, cfg: cfg}
}

func (w *FaultyTaskEventWriter) Calls() int { return w.sched.count() }

// WriteTaskEvent carries no context, so Delay is an unconditional stall — the
// shape of a bbolt fsync on a saturated disk.
func (w *FaultyTaskEventWriter) WriteTaskEvent(event domain.TaskEvent) error {
	if err := gate(nil, w.cfg, &w.sched); err != nil {
		return err
	}
	if w.inner != nil {
		return w.inner.WriteTaskEvent(event)
	}
	return nil
}

var (
	_ domain.VectorStore       = (*FaultyVectorStore)(nil)
	_ domain.Generator         = (*FaultyGenerator)(nil)
	_ domain.BatchEmbedder     = (*FaultyEmbedder)(nil)
	_ domain.InterviewSearcher = (*FaultyInterviewSearcher)(nil)
)
