package memory

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// EdgeBatcher batches the LLM-based entity+relation extraction that runs on
// every IngestMemory. Without batching, every remember() makes one LLM call
// (1-2s) before returning; with batching, the queue absorbs the writes and
// a single LLM call extracts entities+relations for the whole batch —
// roughly N× faster, with N = batch size (default 32).
//
// Lifecycle mirrors the existing Tier-2 pattern (Agent.StartTier2Drain /
// StopTier2Drain): Start launches the drain goroutine, Stop flushes the
// tail and returns. Start is idempotent (a no-op if already running).
//
// Enqueue is non-blocking by design: if the bounded channel is full, the
// doc is still saved (the durability path is the caller's responsibility)
// and a counter ticks up. The graph is the lossy layer here; the doc is
// not.
type EdgeBatcher struct {
	extractor *EdgeExtractor
	writer    *EdgeWriter

	queue   chan *domain.Document
	pending []*domain.Document
	mu      sync.Mutex

	batchSize int
	maxIdle   time.Duration
	llmTimeout time.Duration

	stopCh   chan struct{}
	stopOnce sync.Once
	doneCh   chan struct{}
	started  atomic.Bool

	enqueued uint64 // total docs enqueued
	dropped  uint64 // dropped because the queue was full
	drained  uint64 // total docs drained (for which the LLM ran)
	llmCalls uint64 // total LLM calls made
}

// EdgeBatcherConfig is the constructor input. All fields required.
type EdgeBatcherConfig struct {
	QueueSize  int           // bounded channel size
	BatchSize  int           // load-driven drain trigger
	MaxIdle    time.Duration // time-driven drain trigger
	LLMTimeout time.Duration // per-batch LLM call timeout
}

// NewEdgeBatcher builds the batcher but does NOT start the drain goroutine.
// Call Start(ctx) to begin processing.
func NewEdgeBatcher(extractor *EdgeExtractor, writer *EdgeWriter, cfg EdgeBatcherConfig) *EdgeBatcher {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 1024
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 32
	}
	if cfg.MaxIdle <= 0 {
		cfg.MaxIdle = 2 * time.Second
	}
	if cfg.LLMTimeout <= 0 {
		cfg.LLMTimeout = 30 * time.Second
	}
	return &EdgeBatcher{
		extractor:  extractor,
		writer:     writer,
		queue:      make(chan *domain.Document, cfg.QueueSize),
		batchSize:  cfg.BatchSize,
		maxIdle:    cfg.MaxIdle,
		llmTimeout: cfg.LLMTimeout,
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
	}
}

// Enqueue pushes doc onto the batch queue. Non-blocking. If the queue is
// full (backpressure), the doc is dropped from the graph layer (the
// durability path is the caller's responsibility) and a counter ticks up.
func (b *EdgeBatcher) Enqueue(doc *domain.Document) {
	if b == nil || doc == nil {
		return
	}
	select {
	case b.queue <- doc:
		atomic.AddUint64(&b.enqueued, 1)
	default:
		atomic.AddUint64(&b.dropped, 1)
		slog.Warn("EdgeBatcher: queue full, dropping enrichment",
			"doc_id", doc.ID, "queue_size", cap(b.queue))
	}
}

// Start launches the drain goroutine. Idempotent: a second call is a no-op
// (we only ever have one drainLoop goroutine, which closes doneCh once).
func (b *EdgeBatcher) Start(ctx context.Context) {
	if b == nil {
		return
	}
	if !b.started.CompareAndSwap(false, true) {
		return
	}
	go b.drainLoop(context.WithoutCancel(ctx))
}

// Stop signals the drain goroutine to flush the tail and exit. Blocks
// until the goroutine has flushed and returned.
func (b *EdgeBatcher) Stop() {
	if b == nil {
		return
	}
	b.stopOnce.Do(func() { close(b.stopCh) })
	<-b.doneCh
}

// Stats returns (enqueued, dropped, drained, llmCalls) — for the operator feed.
func (b *EdgeBatcher) Stats() (enqueued, dropped, drained, llmCalls uint64) {
	return atomic.LoadUint64(&b.enqueued),
		atomic.LoadUint64(&b.dropped),
		atomic.LoadUint64(&b.drained),
		atomic.LoadUint64(&b.llmCalls)
}

// drainLoop is the long-running consumer. The key invariant: the channel
// is drained non-blockingly into the pending buffer BEFORE any blocking
// LLM call. Without this, a slow LLM (the hosted reasoning model is
// 60-90s per call) leaves the channel unread, the bounded queue fills,
// and Enqueue drops the graph enrichment. With this, Enqueue only sees
// backpressure when pending is itself at a sane cap.
//
// The loop has three phases per iteration:
//   1. DrainQueueToPending: pull every available item off the channel.
//   2. If pending >= batchSize: call the LLM (blocking).
//   3. Otherwise: wait for more items / idle / stop / ctx-done.
func (b *EdgeBatcher) drainLoop(ctx context.Context) {
	defer close(b.doneCh)
	timer := time.NewTimer(b.maxIdle)
	defer timer.Stop()

	stopRequested := func() {
		// Pull any remaining items from the queue first so the final drain
		// sees them; then drain the pending buffer.
		b.drainQueueToPending()
		b.drain(ctx)
	}

	for {
		b.drainQueueToPending()

		b.mu.Lock()
		n := len(b.pending)
		b.mu.Unlock()
		if n >= b.batchSize {
			b.drain(ctx)
			resetTimer(timer, b.maxIdle)
			continue
		}

		// Pending is below batchSize. Wait for items, the idle timer, stop,
		// or ctx-done. After waking, the loop re-enters at drainQueueToPending.
		select {
		case <-b.stopCh:
			stopRequested()
			return
		case <-ctx.Done():
			stopRequested()
			return
		case doc := <-b.queue:
			b.appendPending(doc)
		case <-timer.C:
			// Idle: flush whatever we have (could be 0).
			if n > 0 {
				b.drain(ctx)
			}
			resetTimer(timer, b.maxIdle)
		}
	}
}

// drainQueueToPending pulls every available item off the channel into
// the pending buffer, non-blocking. Called at the top of every loop
// iteration so the channel is never left unread while the LLM is busy.
func (b *EdgeBatcher) drainQueueToPending() {
	for {
		select {
		case doc := <-b.queue:
			b.appendPending(doc)
		default:
			return
		}
	}
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// appendPending adds doc to the buffer. Caller must NOT hold the mutex
// (the buffer is only touched from this goroutine after the initial
// append from the queue channel).
func (b *EdgeBatcher) appendPending(doc *domain.Document) {
	b.pending = append(b.pending, doc)
}

// drain consumes at most batchSize items from the pending buffer in one LLM
// call. The loop's natural behavior handles the rest: when pending > batchSize,
// the next iteration of drainLoop will call drain again. This bounds the LLM
// prompt size — without it, a 4000-doc pending would all go to the LLM in
// one call and time out. Best-effort: an LLM error is logged and the batch
// is dropped (docs are still saved upstream; only the graph enrichment is
// lost).
func (b *EdgeBatcher) drain(parentCtx context.Context) {
	b.mu.Lock()
	if len(b.pending) == 0 {
		b.mu.Unlock()
		return
	}
	n := b.batchSize
	if n > len(b.pending) {
		n = len(b.pending)
	}
	batch := make([]*domain.Document, n)
	copy(batch, b.pending[:n])
	b.pending = b.pending[n:]
	b.mu.Unlock()

	atomic.AddUint64(&b.drained, uint64(len(batch)))

	// Gather the fact texts (skipping blank ones; the output array is
	// positional so blank-text positions get an empty extraction).
	facts := make([]string, len(batch))
	for i, doc := range batch {
		facts[i] = doc.Text
	}

	llmCtx, cancel := context.WithTimeout(parentCtx, b.llmTimeout)
	defer cancel()
	extractions := b.extractor.ExtractBatch(llmCtx, facts)
	atomic.AddUint64(&b.llmCalls, 1)

	// Materialize the edges for each (doc, extraction) pair. The writer
	// owns the per-doc write cost (graph + index + embedding); it's
	// best-effort and never fails the batch.
	writes := 0
	for i, doc := range batch {
		ext := Extraction{}
		if i < len(extractions) {
			ext = extractions[i]
		}
		b.writer.WriteExtraction(parentCtx, doc, ext)
		if len(ext.Entities) > 0 {
			writes++
		}
	}
	slog.Info("EdgeBatcher: drained batch",
		"batch_size", len(batch), "with_entities", writes, "llm_calls_total", atomic.LoadUint64(&b.llmCalls))
}
