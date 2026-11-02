package memory

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// streamingGenerator is the optional streaming capability a Generator may
// implement. The batcher prefers it over Generate because streaming responses
// are NOT subject to http.Client.Timeout (the streaming client omits the timeout
// so a slow reasoning model can stream its body without being killed
// mid-response). The fallback Generate is fine for test fakes and local Ollama.
//
// Moved here from edge_extractor.go, which was deleted with the original ADR-0053 D2
// LLM edge lane; this batcher is now its only implementer-facing consumer.
type streamingGenerator interface {
	GenerateStream(ctx context.Context, prompt string) (<-chan domain.StreamChunk, error)
}

// ChunkTripletsBatcher batches the LLM-based per-chunk (h, r, t) extraction
// that back-fills the chunk_triplets table (ADR-0053 Phase 0). Without
// batching, every chunk makes one LLM call (1-2s); with batching, a single
// LLM call extracts triplets for the whole batch — roughly N× faster, with
// N = batch size (default 16, matching the EdgeBatcher config).
//
// The batcher uses the same streaming-or-Generate pattern as the
// EdgeExtractor: the LLM is invoked once per drain with a batched prompt
// that emits one `<h##r##t>$$<h##r##t>` segment per input, separated by a
// `---CHUNK---` marker. The parser splits the response back into the
// per-chunk triplet list.
//
// Lifecycle mirrors EdgeBatcher: Start launches the drain goroutine, Stop
// flushes the tail and returns. Start is idempotent. Enqueue is non-blocking;
// on queue-full the doc is dropped from the enrichment layer (the doc itself
// is already saved upstream — see ingest paths).
type ChunkTripletsBatcher struct {
	gen   domain.Generator
	store domain.ChunkTripletsStore
	// extractor is the triplet-extraction port (ADR-0053 D2 revised). Defaults
	// to the LLM adapter; main.go swaps in the kg_extractor system-agent adapter
	// when the deterministic tiered pipeline is enabled. The batching/persistence
	// machinery is unchanged — only the producer of triplets differs.
	extractor domain.TripletExtractor

	queue   chan tripletItem
	pending []tripletItem
	// waitedPending counts pending items that carry a TripletsWait. While it is
	// non-zero the drain loop drains at once instead of waiting for a full batch
	// or the idle timer: a sync ingest is blocked on those items
	// (execution.ingestion.sync_ingest_wait_for_kg). Guarded by mu like pending.
	waitedPending int
	mu            sync.Mutex

	// closeMu + closed make "enqueue after the drain loop exited" impossible to
	// lose silently: enqueue sends (non-blocking) under the read lock, and the
	// drain loop takes the write lock to mark the batcher closed before it
	// releases whatever is still queued. Without it a tracked item could land in
	// the channel after the final sweep and its waiter would block until timeout.
	closeMu sync.RWMutex
	closed  bool

	batchSize  int
	maxIdle    time.Duration
	llmTimeout time.Duration
	// maxRequeues bounds how many times a chunk whose batch failed because the
	// extractor was UNAVAILABLE is put back into pending before it settles as
	// failed (execution.ingestion.kg_triplets_max_requeues).
	maxRequeues int

	// probeCancel cancels the start-up readiness probe, so Stop during a cold
	// extractor boot does not wait out the probe's retries. Set by Start.
	probeCancel context.CancelFunc

	stopCh   chan struct{}
	stopOnce sync.Once
	doneCh   chan struct{}
	started  atomic.Bool

	enqueued uint64 // total chunks enqueued
	dropped  uint64 // dropped because the queue was full
	drained  uint64 // total chunks drained (for which the LLM ran)
	llmCalls uint64 // total LLM calls made
	triplets uint64 // total triplets persisted
	// extractFailed counts chunks settled as failed because extraction errored
	// (after any re-queues); requeued counts re-queue events.
	extractFailed uint64
	requeued      uint64
}

// ChunkTripletsBatcherConfig is the constructor input. All fields required.
type ChunkTripletsBatcherConfig struct {
	QueueSize  int           // bounded channel size
	BatchSize  int           // load-driven drain trigger
	MaxIdle    time.Duration // time-driven drain trigger
	LLMTimeout time.Duration // per-batch LLM call timeout
	// MaxRequeues is how many times a chunk whose batch failed with
	// domain.ErrTripletExtractorUnavailable is re-queued before it settles as
	// failed. 0 = never re-queue; negative is treated as 0.
	MaxRequeues int
}

// NewChunkTripletsBatcher builds the batcher but does NOT start the drain
// goroutine. Call Start(ctx) to begin processing.
func NewChunkTripletsBatcher(gen domain.Generator, store domain.ChunkTripletsStore, cfg ChunkTripletsBatcherConfig) *ChunkTripletsBatcher {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 4096
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 16
	}
	if cfg.MaxIdle <= 0 {
		cfg.MaxIdle = 2 * time.Second
	}
	if cfg.LLMTimeout <= 0 {
		cfg.LLMTimeout = 5 * time.Minute
	}
	if cfg.MaxRequeues < 0 {
		cfg.MaxRequeues = 0
	}
	return &ChunkTripletsBatcher{
		gen:         gen,
		store:       store,
		extractor:   &llmTripletExtractor{gen: gen}, // default: LLM residue tier
		queue:       make(chan tripletItem, cfg.QueueSize),
		batchSize:   cfg.BatchSize,
		maxIdle:     cfg.MaxIdle,
		llmTimeout:  cfg.LLMTimeout,
		maxRequeues: cfg.MaxRequeues,
		stopCh:      make(chan struct{}),
		doneCh:      make(chan struct{}),
	}
}

// UseExtractor swaps the triplet-extraction adapter (ADR-0053 D2 revised). Call
// before Start. nil is ignored (keeps the default LLM extractor). This is how
// main.go injects the kg_extractor system-agent (metadata + spacy_patterns)
// adapter onto the ingest hot path.
func (b *ChunkTripletsBatcher) UseExtractor(e domain.TripletExtractor) {
	if b == nil || e == nil {
		return
	}
	b.extractor = e
}

// Enqueue pushes doc onto the batch queue. Non-blocking. If the queue is
// full (backpressure), the chunk's triplet extraction is dropped (the doc
// itself is already saved upstream) and a counter ticks up.
func (b *ChunkTripletsBatcher) Enqueue(doc *domain.Document) {
	if b == nil || doc == nil {
		return
	}
	b.enqueue(tripletItem{doc: doc})
}

// EnqueueTracked enqueues one document's chunks and returns a TripletsWait that
// completes when every one of them has been extracted and its triplets
// persisted, or has definitively failed (dropped on a full queue, a save error,
// or the batcher stopping). It is the read-your-writes half of
// execution.ingestion.sync_ingest_wait_for_kg (owner decision 2026-11-02):
// ProcessSync waits on it so a query issued after the ingest returns sees that
// document's chunk_triplets.
//
// Enqueueing stays non-blocking and tracked items share the queue with
// untracked ones, so the batcher still batches across documents; the only
// behavioural difference is that a pending tracked item makes the drain loop
// drain at once rather than wait for a full batch or the idle timer. nil docs
// are skipped. A nil batcher returns a wait that is already complete.
func (b *ChunkTripletsBatcher) EnqueueTracked(docs []*domain.Document) *TripletsWait {
	n := 0
	for _, d := range docs {
		if d != nil {
			n++
		}
	}
	w := newTripletsWait(n)
	if b == nil {
		w.settle(n, false)
		return w
	}
	for _, d := range docs {
		if d != nil {
			b.enqueue(tripletItem{doc: d, wait: w})
		}
	}
	return w
}

func (b *ChunkTripletsBatcher) enqueue(it tripletItem) {
	b.closeMu.RLock()
	defer b.closeMu.RUnlock()
	if b.closed {
		// The drain loop has exited: nothing will ever read this item.
		atomic.AddUint64(&b.dropped, 1)
		it.wait.stop(1)
		return
	}
	select {
	case b.queue <- it:
		atomic.AddUint64(&b.enqueued, 1)
	default:
		atomic.AddUint64(&b.dropped, 1)
		it.wait.settle(1, false)
		slog.Warn("ChunkTripletsBatcher: queue full, dropping enrichment",
			"doc_id", it.doc.ID, "queue_size", cap(b.queue))
	}
}

// Start launches the drain goroutine. Idempotent: a second call is a no-op.
func (b *ChunkTripletsBatcher) Start(ctx context.Context) {
	if b == nil {
		return
	}
	if !b.started.CompareAndSwap(false, true) {
		return
	}
	base := context.WithoutCancel(ctx)
	probeCtx, cancel := context.WithCancel(base)
	b.mu.Lock()
	b.probeCancel = cancel
	b.mu.Unlock()
	go b.drainLoop(base, probeCtx)
}

// Stop signals the drain goroutine to flush the tail and exit. Blocks
// until the goroutine has flushed and returned.
func (b *ChunkTripletsBatcher) Stop() {
	if b == nil {
		return
	}
	b.stopOnce.Do(func() {
		b.mu.Lock()
		cancel := b.probeCancel
		b.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		close(b.stopCh)
	})
	<-b.doneCh
}

// FailureStats returns (extractFailed, requeued): chunks settled as failed
// because extraction errored, and re-queue events — for the operator feed and
// tests (T-20261102-kg-extractor-cold-start-drops-the-first-kg).
func (b *ChunkTripletsBatcher) FailureStats() (extractFailed, requeued uint64) {
	return atomic.LoadUint64(&b.extractFailed), atomic.LoadUint64(&b.requeued)
}

// probeReady runs once, at the top of the drain goroutine, when the extractor
// can report readiness (domain.ReadinessProber — the kg_extractor dispatcher).
// It boots the lazily started extractor agent at kernel start instead of inside
// the first ingest's batch, and says so: "KG extractor READY", or a WARN with
// the cause. Items enqueued meanwhile wait in the queue, so the first batches
// wait for the agent — bounded by the probe's own retries and the batch timeout
// — and then fail visibly if it never comes up. It never blocks kernel boot (it
// runs on the drain goroutine), and Stop cancels it.
func (b *ChunkTripletsBatcher) probeReady(ctx context.Context) {
	p, ok := b.extractor.(domain.ReadinessProber)
	if !ok {
		return
	}
	t := time.Now()
	pctx, cancel := context.WithTimeout(ctx, b.llmTimeout)
	defer cancel()
	if err := p.Ready(pctx); err != nil {
		slog.Warn("KG extractor NOT READY: chunk-triplet extraction will retry per batch; failed chunks settle as kg_failed",
			"elapsed_ms", time.Since(t).Milliseconds(), "cause", err)
		return
	}
	slog.Info("KG extractor READY", "elapsed_ms", time.Since(t).Milliseconds())
}

// Stats returns (enqueued, dropped, drained, llmCalls, tripletsPersisted) —
// for the operator feed.
func (b *ChunkTripletsBatcher) Stats() (enqueued, dropped, drained, llmCalls, triplets uint64) {
	return atomic.LoadUint64(&b.enqueued),
		atomic.LoadUint64(&b.dropped),
		atomic.LoadUint64(&b.drained),
		atomic.LoadUint64(&b.llmCalls),
		atomic.LoadUint64(&b.triplets)
}

// drainLoop is the long-running consumer. Same invariant as EdgeBatcher:
// the channel is drained non-blockingly into the pending buffer BEFORE any
// blocking LLM call. Without this, a slow LLM (the hosted reasoning model
// can be 60-180s per call) leaves the channel unread, the bounded queue
// fills, and Enqueue drops the enrichment. With this, Enqueue only sees
// backpressure when pending is itself at a sane cap.
func (b *ChunkTripletsBatcher) drainLoop(ctx, probeCtx context.Context) {
	defer close(b.doneCh)
	b.probeReady(probeCtx)
	timer := time.NewTimer(b.maxIdle)
	defer timer.Stop()

	stopRequested := func() {
		b.drainQueueToPending()
		b.drain(ctx)
		b.releaseRemaining()
	}

	for {
		b.drainQueueToPending()

		b.mu.Lock()
		n := len(b.pending)
		waited := b.waitedPending
		b.mu.Unlock()
		// A tracked item means a sync ingest is blocked on it: drain now, with
		// whatever has accumulated (up to batchSize), instead of idling.
		if n >= b.batchSize || waited > 0 {
			b.drain(ctx)
			resetTripletsTimer(timer, b.maxIdle)
			continue
		}

		select {
		case <-b.stopCh:
			stopRequested()
			return
		case <-ctx.Done():
			stopRequested()
			return
		case it := <-b.queue:
			b.appendPending(it)
		case <-timer.C:
			if n > 0 {
				b.drain(ctx)
			}
			resetTripletsTimer(timer, b.maxIdle)
		}
	}
}

func resetTripletsTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

func (b *ChunkTripletsBatcher) drainQueueToPending() {
	for {
		select {
		case it := <-b.queue:
			b.appendPending(it)
		default:
			return
		}
	}
}

func (b *ChunkTripletsBatcher) appendPending(it tripletItem) {
	b.mu.Lock()
	b.pending = append(b.pending, it)
	if it.wait != nil {
		b.waitedPending++
	}
	b.mu.Unlock()
}

// releaseRemaining runs once, as the drain loop exits. The loop keeps draining
// while a full batch or a tracked item is pending, and the exit path flushes one
// more batch (the existing tail behaviour); anything still pending or queued
// after that is not extracted, and any sync ingest waiting on it is released with
// ErrTripletsBatcherStopped instead of blocking until its timeout. Marking the
// batcher closed first (under the write lock) guarantees no tracked item can be
// enqueued after the final sweep.
func (b *ChunkTripletsBatcher) releaseRemaining() {
	b.closeMu.Lock()
	b.closed = true
	b.closeMu.Unlock()
	b.drainQueueToPending()
	b.mu.Lock()
	rest := b.pending
	b.pending = nil
	b.waitedPending = 0
	b.mu.Unlock()
	for _, it := range rest {
		it.wait.stop(1)
	}
}

// drain consumes at most batchSize items from the pending buffer in one LLM
// call. The loop's natural behavior handles the rest: when pending > batchSize,
// the next iteration of drainLoop will call drain again. This bounds the LLM
// prompt size — without it, a 4000-chunk pending would all go to the LLM in
// one call and time out. Best-effort: an LLM error is logged and the batch is
// dropped (chunks are still saved upstream; only the triplets are lost).
func (b *ChunkTripletsBatcher) drain(parentCtx context.Context) {
	b.mu.Lock()
	if len(b.pending) == 0 {
		b.mu.Unlock()
		return
	}
	n := b.batchSize
	if n > len(b.pending) {
		n = len(b.pending)
	}
	batch := make([]tripletItem, n)
	copy(batch, b.pending[:n])
	b.pending = b.pending[n:]
	for _, it := range batch {
		if it.wait != nil {
			b.waitedPending--
		}
	}
	b.mu.Unlock()

	atomic.AddUint64(&b.drained, uint64(len(batch)))

	// Gather the chunk texts (skipping blank ones; the output array is
	// positional so blank-text positions get an empty extraction).
	texts := make([]string, len(batch))
	ids := make([]string, len(batch))
	for i, it := range batch {
		texts[i] = it.doc.Text
		ids[i] = it.doc.ID
	}

	llmCtx, cancel := context.WithTimeout(parentCtx, b.llmTimeout)
	defer cancel()
	tripletsPerChunk, extractErr := b.extractor.ExtractBatch(llmCtx, texts, ids)
	atomic.AddUint64(&b.llmCalls, 1)
	if extractErr != nil {
		b.failBatch(batch, extractErr)
		return
	}

	// Persist each chunk's triplets. Best-effort: a per-chunk save error
	// is logged and the enrichment for that chunk is dropped; the doc
	// itself is already saved upstream.
	//
	// Every item of the batch is settled exactly once, whether it produced
	// triplets or not: a chunk with no extractable triplets is COMPLETE (there is
	// nothing to read), and an extractor that returned fewer rows than the batch
	// leaves the tail complete-and-empty, as before.
	saved := 0
	for i, it := range batch {
		var triplets []domain.ChunkTriplet
		if i < len(tripletsPerChunk) {
			triplets = tripletsPerChunk[i]
		}
		if len(triplets) == 0 {
			it.wait.settle(1, true)
			continue
		}
		if err := b.store.SaveChunkTriplets(parentCtx, it.doc.ID, triplets); err != nil {
			slog.Warn("ChunkTripletsBatcher: save failed",
				"doc_id", it.doc.ID, "err", err, "triplets", len(triplets))
			it.wait.settle(1, false)
			continue
		}
		saved++
		atomic.AddUint64(&b.triplets, uint64(len(triplets)))
		it.wait.settle(1, true)
	}
	slog.Info("ChunkTripletsBatcher: drained batch",
		"batch_size", len(batch), "with_triplets", saved, "llm_calls_total", atomic.LoadUint64(&b.llmCalls))
}

// failBatch handles a batch the extractor did not extract
// (T-20261102-kg-extractor-cold-start-drops-the-first-kg). Before the port could
// return an error such a batch settled every chunk as complete-and-empty, so a
// cold-start boot failure was indistinguishable from "no triplets in this text".
//
// When the extractor was unavailable (domain.ErrTripletExtractorUnavailable) a
// chunk still under maxRequeues goes back to the tail of pending for another
// pass; everything else settles as FAILED, so a sync ingest waiting on it
// reports kg_failed. Re-queueing only re-appends items already accepted, so
// pending cannot grow beyond what was enqueued, and no goroutine is started.
// One WARN per batch, with the cause — never one per chunk.
func (b *ChunkTripletsBatcher) failBatch(batch []tripletItem, err error) {
	retryable := errors.Is(err, domain.ErrTripletExtractorUnavailable)
	var again []tripletItem
	failed := 0
	for _, it := range batch {
		if retryable && it.attempts < b.maxRequeues {
			it.attempts++
			again = append(again, it)
			continue
		}
		failed++
		it.wait.settle(1, false)
	}
	if len(again) > 0 {
		b.mu.Lock()
		for _, it := range again {
			b.pending = append(b.pending, it)
			if it.wait != nil {
				b.waitedPending++
			}
		}
		b.mu.Unlock()
		atomic.AddUint64(&b.requeued, uint64(len(again)))
	}
	atomic.AddUint64(&b.extractFailed, uint64(failed))
	slog.Warn("ChunkTripletsBatcher: triplet extraction failed for a batch",
		"batch_size", len(batch), "requeued", len(again), "failed", failed,
		"max_requeues", b.maxRequeues, "unavailable", retryable, "cause", err,
		"effect", "failed chunks have no KG triplets (kg_failed); their content is stored")
}

// buildBatchTripletPrompt builds the batched extraction prompt. Format:
//
//	"Chunk 1: <chunk1>
//	 Triplets 1: <h##r##t>$$<h##r##t>...
//	 Chunk 2: <chunk2>
//	 Triplets 2: ..."
//
// The examples use "Text N:" labels (the KG²RAG reference shape) so the
// in-context demos are grounded; the actual input uses "Chunk N:" to keep
// the chunk/triplet labels distinct from the in-prompt example text, which
// avoids the parser confusing example bodies with real output.
func buildBatchTripletPrompt(texts []string) string {
	var sb strings.Builder
	sb.WriteString("Extract informative triplets from the chunks following the examples. ")
	sb.WriteString("Make sure the triplet texts are only directly from the given text! ")
	sb.WriteString("Complete directly and strictly following the instructions without any additional words, line break nor space.\n\n")

	// Two examples
	sb.WriteString("Text 1: Scott Derrickson (born July 16, 1966) is an American director.\n")
	sb.WriteString("Triplets 1: <Scott Derrickson##born in##1966>$$<Scott Derrickson##nationality##America>$$<Scott Derrickson##occupation##director>\n\n")
	sb.WriteString("Text 2: A Kiss for Corliss is a 1949 American comedy film directed by Richard Wallace.\n")
	sb.WriteString("Triplets 2: <A Kiss for Corliss##directed by##Richard Wallace>$$<A Kiss for Corliss##genre##comedy>\n\n")

	for i, t := range texts {
		sb.WriteString("Chunk ")
		sb.WriteString(itoa(i + 1))
		sb.WriteString(": ")
		sb.WriteString(t)
		sb.WriteString("\nTriplets ")
		sb.WriteString(itoa(i + 1))
		sb.WriteString(": ")
	}
	return sb.String()
}

// itoa is a tiny dependency-free integer formatter.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// callTripletsLLM invokes the LLM, preferring streaming when the Generator
// supports it. The streaming path concatenates chunks and returns the full
// text; it is NOT subject to http.Client.Timeout, so a slow reasoning model
// can stream its body without being killed mid-response.
func callTripletsLLM(ctx context.Context, gen domain.Generator, prompt string) (string, error) {
	if sg, ok := gen.(streamingGenerator); ok {
		return callTripletsLLMStream(ctx, sg, prompt)
	}
	return gen.Generate(ctx, prompt)
}

func callTripletsLLMStream(ctx context.Context, sg streamingGenerator, prompt string) (string, error) {
	ch, err := sg.GenerateStream(ctx, prompt)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for c := range ch {
		if c.Text != "" {
			if strings.HasPrefix(c.Text, "stream_error: ") {
				return "", &tripletsStreamError{msg: strings.TrimPrefix(c.Text, "stream_error: ")}
			}
			sb.WriteString(c.Text)
		}
		if c.IsFinal {
			break
		}
	}
	return sb.String(), nil
}

type tripletsStreamError struct{ msg string }

func (e *tripletsStreamError) Error() string { return e.msg }

// parseBatchedTripletResponse splits the LLM's batched response back into
// per-chunk triplet lists. The expected format is:
//
//	"Triplets 1: <h##r##t>$$<h##r##t>...
//	 Triplets 2: <h##r##t>$$..."
//
// The LLM may ramble or use a slightly different prefix; we use a permissive
// regex.
func parseBatchedTripletResponse(resp string, n int) [][]domain.ChunkTriplet {
	out := make([][]domain.ChunkTriplet, n)
	// Split on "Triplets <i>:" prefixes. We look for the marker; if the LLM
	// wrote something else (e.g. "Triplets for text 1:") we still try.
	type segment struct {
		idx  int
		body string
	}
	var segs []segment
	// Walk the response looking for "Triplets N:" or "Triplets\nN:" or
	// "Triplets:" markers. Take the simplest: split on `Triplets ` and
	// parse the trailing digit prefix.
	marker := []byte("Triplets ")
	pos := 0
	for {
		at := indexAt(resp, marker, pos)
		if at < 0 {
			break
		}
		// Look for a number right after "Triplets ".
		after := at + len(marker)
		i := 0
		hasDigit := false
		for after+i < len(resp) && resp[after+i] >= '0' && resp[after+i] <= '9' {
			i++
			hasDigit = true
		}
		if !hasDigit {
			pos = at + len(marker)
			continue
		}
		// Parse the integer.
		num := 0
		for j := 0; j < i; j++ {
			num = num*10 + int(resp[after+j]-'0')
		}
		// Skip the digit run + a colon / space if present.
		bodyStart := after + i
		for bodyStart < len(resp) && (resp[bodyStart] == ':' || resp[bodyStart] == ' ') {
			bodyStart++
		}
		// Find the next "Triplets " marker at the same depth — but the LLM
		// can also use "Text N:" between segments, so we also break on that.
		bodyEnd := len(resp)
		next := indexAt(resp, marker, bodyStart)
		if next >= 0 {
			bodyEnd = next
		}
		nextText := indexAt(resp, []byte("Text "), bodyStart)
		if nextText >= 0 && nextText < bodyEnd {
			bodyEnd = nextText
		}
		segs = append(segs, segment{idx: num - 1, body: resp[bodyStart:bodyEnd]})
		pos = bodyStart
	}
	// If the LLM emitted zero "Triplets N:" markers (a degraded response),
	// treat the entire response as chunk 0.
	if len(segs) == 0 && strings.TrimSpace(resp) != "" {
		segs = []segment{{idx: 0, body: resp}}
	}
	for _, s := range segs {
		if s.idx < 0 || s.idx >= n {
			continue
		}
		out[s.idx] = parseChunkTripletOutput(s.body)
	}
	return out
}

func indexAt(s string, sub []byte, from int) int {
	// bytes-aware indexOf. We do it by hand because strings.Index doesn't
	// take a starting offset. Used in a hot path; keep it simple.
	if from >= len(s) {
		return -1
	}
	for i := from; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == string(sub) {
			return i
		}
	}
	return -1
}
