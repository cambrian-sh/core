package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// T-20261102-kg-extractor-cold-start-drops-the-first-kg: an extraction FAILURE
// must settle the batch's chunks as failed (kg_failed), not as complete-and-empty,
// and an unavailable extractor is re-queued a bounded number of times.

// scriptedExtractor fails its first failFirst calls with err, then emits one
// triplet per chunk. calls counts every ExtractBatch.
type scriptedExtractor struct {
	failFirst int64
	err       error
	calls     atomic.Int64
	empty     bool // succeed with no triplets instead of one per chunk
}

func (s *scriptedExtractor) ExtractBatch(_ context.Context, texts, ids []string) ([][]domain.ChunkTriplet, error) {
	n := s.calls.Add(1)
	if n <= s.failFirst {
		return nil, s.err
	}
	out := make([][]domain.ChunkTriplet, len(texts))
	if s.empty {
		return out, nil
	}
	for i := range texts {
		out[i] = []domain.ChunkTriplet{{H: ids[i], R: "mentions", T: "x"}}
	}
	return out, nil
}

var errUnavailable = fmt.Errorf("boot agent kg_extractor_agent: socket did not appear: %w", domain.ErrTripletExtractorUnavailable)

func newFailureBatcher(ext domain.TripletExtractor, store domain.ChunkTripletsStore, requeues int) *ChunkTripletsBatcher {
	b := NewChunkTripletsBatcher(&streamingBatchGen{}, store, ChunkTripletsBatcherConfig{
		QueueSize: 64, BatchSize: 16, MaxIdle: time.Hour, LLMTimeout: 5 * time.Second, MaxRequeues: requeues,
	})
	b.UseExtractor(ext)
	return b
}

func waitCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// captureWarn routes slog to a buffer at WARN and returns a reader for it.
func captureWarn(t *testing.T) func() string {
	t.Helper()
	var buf bytes.Buffer
	var mu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&buf, &mu}, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}

// An extraction error settles every chunk of the batch as FAILED; a successful
// empty extraction settles it as complete. That is the distinction the port
// lacked.
func TestChunkTripletsBatcher_ExtractionErrorIsFailedNotEmpty(t *testing.T) {
	store := newLockedTripletStore()
	b := newFailureBatcher(&scriptedExtractor{failFirst: 1 << 30, err: errUnavailable}, store, 0)
	b.Start(context.Background())
	w := b.EnqueueTracked(chunkDocs("f", 3))
	if err := w.Wait(waitCtx(t)); err != nil {
		t.Fatalf("wait: %v", err)
	}
	b.Stop()
	if w.Failed() != 3 {
		t.Fatalf("failed = %d, want 3: an extraction error must not settle as complete", w.Failed())
	}
	if failed, _ := b.FailureStats(); failed != 3 {
		t.Fatalf("FailureStats failed = %d, want 3", failed)
	}

	empty := newFailureBatcher(&scriptedExtractor{empty: true}, newLockedTripletStore(), 0)
	empty.Start(context.Background())
	defer empty.Stop()
	we := empty.EnqueueTracked(chunkDocs("e", 3))
	if err := we.Wait(waitCtx(t)); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if we.Failed() != 0 {
		t.Fatalf("an empty successful extraction settled %d chunks as failed", we.Failed())
	}
}

// Cold start: the extractor is unavailable for the first pass and up for the
// second. With one re-queue allowed, every chunk ends with triplets and nothing
// is failed.
func TestChunkTripletsBatcher_UnavailableRequeuedThenSucceeds(t *testing.T) {
	store := newLockedTripletStore()
	ext := &scriptedExtractor{failFirst: 1, err: errUnavailable}
	b := newFailureBatcher(ext, store, 1)
	b.Start(context.Background())
	defer b.Stop()
	docs := chunkDocs("c", 4)
	w := b.EnqueueTracked(docs)
	if err := w.Wait(waitCtx(t)); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if w.Failed() != 0 {
		t.Fatalf("failed = %d after a successful retry", w.Failed())
	}
	for _, d := range docs {
		if !store.has(d.ID) {
			t.Fatalf("%s has no triplets after the retry", d.ID)
		}
	}
	if failed, requeued := b.FailureStats(); failed != 0 || requeued != 4 {
		t.Fatalf("FailureStats = (%d, %d), want (0, 4)", failed, requeued)
	}
	if ext.calls.Load() != 2 {
		t.Fatalf("extractor calls = %d, want 2", ext.calls.Load())
	}
}

// Re-queues are bounded: a permanently unavailable extractor is tried exactly
// 1+MaxRequeues times per chunk, then the chunks settle as failed.
func TestChunkTripletsBatcher_RequeuesBounded(t *testing.T) {
	ext := &scriptedExtractor{failFirst: 1 << 30, err: errUnavailable}
	b := newFailureBatcher(ext, newLockedTripletStore(), 2)
	b.Start(context.Background())
	defer b.Stop()
	w := b.EnqueueTracked(chunkDocs("p", 5))
	if err := w.Wait(waitCtx(t)); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if w.Failed() != 5 {
		t.Fatalf("failed = %d, want 5", w.Failed())
	}
	if got := ext.calls.Load(); got != 3 {
		t.Fatalf("extractor calls = %d, want 3 (1 + 2 re-queues)", got)
	}
	if failed, requeued := b.FailureStats(); failed != 5 || requeued != 10 {
		t.Fatalf("FailureStats = (%d, %d), want (5, 10)", failed, requeued)
	}
}

// An error that is not "unavailable" (an unusable reply) is not re-queued.
func TestChunkTripletsBatcher_BadReplyNotRequeued(t *testing.T) {
	ext := &scriptedExtractor{failFirst: 1 << 30, err: errors.New("bad response JSON")}
	b := newFailureBatcher(ext, newLockedTripletStore(), 3)
	b.Start(context.Background())
	defer b.Stop()
	w := b.EnqueueTracked(chunkDocs("j", 2))
	if err := w.Wait(waitCtx(t)); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if w.Failed() != 2 || ext.calls.Load() != 1 {
		t.Fatalf("failed=%d calls=%d, want 2 and 1", w.Failed(), ext.calls.Load())
	}
}

// One WARN per failed batch with the cause, never one per chunk.
func TestChunkTripletsBatcher_FailureWarnOncePerBatch(t *testing.T) {
	read := captureWarn(t)
	b := newFailureBatcher(&scriptedExtractor{failFirst: 1 << 30, err: errUnavailable}, newLockedTripletStore(), 0)
	b.Start(context.Background())
	w := b.EnqueueTracked(chunkDocs("w", 10)) // one batch of 10
	if err := w.Wait(waitCtx(t)); err != nil {
		t.Fatalf("wait: %v", err)
	}
	b.Stop()
	out := read()
	if n := strings.Count(out, "triplet extraction failed for a batch"); n != 1 {
		t.Fatalf("WARN count = %d, want 1 per batch:\n%s", n, out)
	}
	if !strings.Contains(out, "batch_size=10") || !strings.Contains(out, "failed=10") || !strings.Contains(out, "socket did not appear") {
		t.Fatalf("WARN lacks batch size, failed count or cause: %q", out)
	}
}

// ProcessSync with the wait on: a failed extraction reaches the ingest-timing
// line and trailer as kg_failed#, and the WARN says extraction_failed.
func TestProcessSync_WaitForKG_ExtractionFailureReportsKGFailed(t *testing.T) {
	read := captureWarn(t)
	b := newFailureBatcher(&scriptedExtractor{failFirst: 1 << 30, err: errUnavailable}, newLockedTripletStore(), 0)
	b.Start(context.Background())
	defer b.Stop()
	im := newKGWaitIngestManager(t, b, true, 0)
	clock := domain.NewStageClock()
	ctx := domain.WithStageClock(context.Background(), clock)
	if _, err := im.ProcessSync(ctx, kgDoc("ingest_memory://kgfail")); err != nil {
		t.Fatalf("an extraction failure must not fail the ingest: %v", err)
	}
	if clock.Counts()[CountKGFailed] < 1 || clock.Counts()[CountKGPending] != 0 {
		t.Fatalf("counts = %v, want kg_failed>=1 and no kg_pending", clock.Counts())
	}
	if line := FormatStageClock(clock, time.Millisecond); !strings.Contains(line, "kg_failed#=") {
		t.Fatalf("trailer lacks kg_failed#: %q", line)
	}
	if out := read(); !strings.Contains(out, "reason=extraction_failed") {
		t.Fatalf("no extraction_failed WARN: %q", out)
	}
}

// probeExtractor implements domain.ReadinessProber. Ready fails until up is
// set; block makes Ready wait for ctx.
type probeExtractor struct {
	scriptedExtractor
	ready  error
	block  bool
	probes atomic.Int64
}

func (p *probeExtractor) Ready(ctx context.Context) error {
	p.probes.Add(1)
	if p.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return p.ready
}

// The drain loop probes a ReadinessProber once at start and logs READY or a
// WARN with the cause; extraction still proceeds either way.
func TestChunkTripletsBatcher_ReadinessProbeLogged(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ready error
		want  string
		level slog.Level
	}{
		{"ready", nil, "KG extractor READY", slog.LevelInfo},
		{"not ready", errUnavailable, "KG extractor NOT READY", slog.LevelWarn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			var mu sync.Mutex
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&buf, &mu}, &slog.HandlerOptions{Level: slog.LevelInfo})))
			defer slog.SetDefault(prev)

			ext := &probeExtractor{ready: tc.ready}
			b := newFailureBatcher(ext, newLockedTripletStore(), 0)
			b.Start(context.Background())
			w := b.EnqueueTracked(chunkDocs("r", 2))
			if err := w.Wait(waitCtx(t)); err != nil {
				t.Fatalf("wait: %v", err)
			}
			b.Stop()
			mu.Lock()
			out := buf.String()
			mu.Unlock()
			if !strings.Contains(out, tc.want) || !strings.Contains(out, "level="+tc.level.String()) {
				t.Fatalf("log lacks %q at %s: %q", tc.want, tc.level, out)
			}
			if tc.ready != nil && !strings.Contains(out, "socket did not appear") {
				t.Fatalf("NOT READY WARN lacks the cause: %q", out)
			}
			if ext.probes.Load() != 1 {
				t.Fatalf("probes = %d, want exactly 1", ext.probes.Load())
			}
		})
	}
}

// Stop during a probe that never returns on its own does not hang: Stop cancels
// the probe (the kernel can shut down during a cold extractor boot).
func TestChunkTripletsBatcher_StopCancelsReadinessProbe(t *testing.T) {
	ext := &probeExtractor{block: true}
	b := newFailureBatcher(ext, newLockedTripletStore(), 0)
	b.Start(context.Background())
	time.Sleep(20 * time.Millisecond)
	done := make(chan struct{})
	go func() { b.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop hung on the readiness probe")
	}
}
