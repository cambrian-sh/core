package memory

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// T-20261102-async-kg-triplet-extraction-30-chunks-s-lags: tests for the
// per-document completion signal of the ChunkTripletsBatcher and the
// sync_ingest_wait_for_kg wait in ProcessSync.

// lockedTripletStore is a concurrency-safe ChunkTripletsStore fake: the batcher
// writes from its drain goroutine while the test reads.
type lockedTripletStore struct {
	mu   sync.Mutex
	rows map[string][]domain.ChunkTriplet
	fail map[string]bool
}

func newLockedTripletStore() *lockedTripletStore {
	return &lockedTripletStore{rows: map[string][]domain.ChunkTriplet{}, fail: map[string]bool{}}
}

func (s *lockedTripletStore) SaveChunkTriplets(_ context.Context, id string, ts []domain.ChunkTriplet) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail[id] {
		return errors.New("save failed")
	}
	s.rows[id] = append(s.rows[id], ts...)
	return nil
}

func (s *lockedTripletStore) has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rows[id]) > 0
}

func (s *lockedTripletStore) ForChunk(_ context.Context, id string) ([]domain.ChunkTriplet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows[id], nil
}

func (s *lockedTripletStore) ForChunks(_ context.Context, ids []string) (map[string][]domain.ChunkTriplet, error) {
	out := map[string][]domain.ChunkTriplet{}
	for _, id := range ids {
		out[id], _ = s.ForChunk(context.Background(), id)
	}
	return out, nil
}

func (s *lockedTripletStore) ChunksMentioningEntity(context.Context, string, int, *domain.TagPredicate) ([]string, error) {
	return nil, nil
}

// gatedExtractor emits one triplet per chunk, but each ExtractBatch call blocks
// until the test releases it, so the test controls batch boundaries in time.
type gatedExtractor struct {
	gate    chan struct{}
	mu      sync.Mutex
	batches [][]string
}

func newGatedExtractor() *gatedExtractor { return &gatedExtractor{gate: make(chan struct{})} }

func (g *gatedExtractor) ExtractBatch(ctx context.Context, texts, ids []string) ([][]domain.ChunkTriplet, error) {
	g.mu.Lock()
	g.batches = append(g.batches, append([]string(nil), ids...))
	g.mu.Unlock()
	select {
	case <-g.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	out := make([][]domain.ChunkTriplet, len(texts))
	for i := range texts {
		out[i] = []domain.ChunkTriplet{{H: ids[i], R: "mentions", T: "x"}}
	}
	return out, nil
}

func (g *gatedExtractor) release() { g.gate <- struct{}{} }

// instantExtractor returns one triplet per chunk immediately.
type instantExtractor struct{}

func (instantExtractor) ExtractBatch(_ context.Context, texts, ids []string) ([][]domain.ChunkTriplet, error) {
	out := make([][]domain.ChunkTriplet, len(texts))
	for i := range texts {
		out[i] = []domain.ChunkTriplet{{H: ids[i], R: "mentions", T: "x"}}
	}
	return out, nil
}

func chunkDocs(prefix string, n int) []*domain.Document {
	out := make([]*domain.Document, n)
	for i := range out {
		out[i] = &domain.Document{ID: prefix + itoa(i), Text: "text " + prefix}
	}
	return out
}

func isDone(w *TripletsWait) bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

// Each document's waiter completes when ITS chunks are persisted, even when
// its chunks share a batch with another document's: A(3)+B(3) with batch size 4
// drain as [A0 A1 A2 B0] then [B1 B2]; A completes after the first batch, B only
// after the second. Untracked chunks ride along in the same batches.
func TestTripletsWait_SignalledPerDocumentAcrossMixedBatches(t *testing.T) {
	store := newLockedTripletStore()
	ext := newGatedExtractor()
	b := NewChunkTripletsBatcher(&streamingBatchGen{}, store, ChunkTripletsBatcherConfig{
		QueueSize: 64, BatchSize: 4, MaxIdle: time.Hour, LLMTimeout: 5 * time.Second,
	})
	b.UseExtractor(ext)

	a := chunkDocs("a", 3)
	bb := chunkDocs("b", 3)
	// Enqueued before Start so the first drain sees all six items.
	wa := b.EnqueueTracked(a)
	wb := b.EnqueueTracked(bb)
	b.Enqueue(&domain.Document{ID: "u0", Text: "untracked"})
	b.Start(context.Background())
	defer b.Stop()

	ext.release() // batch 1
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := wa.Wait(ctx); err != nil {
		t.Fatalf("A wait: %v", err)
	}
	for _, d := range a {
		if !store.has(d.ID) {
			t.Fatalf("A returned before %s was persisted", d.ID)
		}
	}
	if isDone(wb) {
		t.Fatal("B completed although two of its chunks are still unextracted")
	}
	if wb.Pending() != 2 {
		t.Fatalf("B pending = %d, want 2", wb.Pending())
	}
	ext.release() // batch 2: B1 B2 + u0
	if err := wb.Wait(ctx); err != nil {
		t.Fatalf("B wait: %v", err)
	}
	for _, d := range bb {
		if !store.has(d.ID) {
			t.Fatalf("B returned before %s was persisted", d.ID)
		}
	}
	ext.mu.Lock()
	defer ext.mu.Unlock()
	if len(ext.batches) != 2 || len(ext.batches[0]) != 4 || len(ext.batches[1]) != 3 {
		t.Fatalf("batching across documents lost: batches %v", ext.batches)
	}
}

// A tracked item drains at once instead of waiting for a full batch or the idle
// timer (MaxIdle is an hour here); untracked items alone still wait.
func TestTripletsWait_TrackedItemDrainsImmediately(t *testing.T) {
	store := newLockedTripletStore()
	b := NewChunkTripletsBatcher(&streamingBatchGen{}, store, ChunkTripletsBatcherConfig{
		QueueSize: 64, BatchSize: 16, MaxIdle: time.Hour, LLMTimeout: 5 * time.Second,
	})
	b.UseExtractor(instantExtractor{})
	b.Start(context.Background())
	defer b.Stop()

	b.Enqueue(&domain.Document{ID: "async0", Text: "x"})
	time.Sleep(50 * time.Millisecond)
	if store.has("async0") {
		t.Fatal("an untracked item drained before a full batch or the idle timer: async batching changed")
	}
	w := b.EnqueueTracked(chunkDocs("s", 2))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := w.Wait(ctx); err != nil {
		t.Fatalf("tracked wait: %v", err)
	}
	if !store.has("s0") || !store.has("s1") || !store.has("async0") {
		t.Fatal("the immediate drain must take the pending untracked item along in the same batch")
	}
}

// Cancelling the waiter's ctx ends the wait, not the extraction: the chunks are
// still extracted afterwards.
func TestTripletsWait_CtxCancelStopsWaitNotExtraction(t *testing.T) {
	store := newLockedTripletStore()
	ext := newGatedExtractor()
	b := NewChunkTripletsBatcher(&streamingBatchGen{}, store, ChunkTripletsBatcherConfig{
		QueueSize: 64, BatchSize: 16, MaxIdle: time.Hour, LLMTimeout: 5 * time.Second,
	})
	b.UseExtractor(ext)
	b.Start(context.Background())
	defer b.Stop()

	w := b.EnqueueTracked(chunkDocs("c", 2))
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- w.Wait(ctx) }()
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait after cancel = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after ctx cancel")
	}
	ext.release()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if err := w.Wait(ctx2); err != nil {
		t.Fatalf("extraction did not complete after the waiter left: %v", err)
	}
	if !store.has("c0") || !store.has("c1") {
		t.Fatal("chunks not extracted after the waiter's ctx was cancelled")
	}
}

// Stop drains the backlog it finds (the batcher's existing shutdown behaviour),
// so the waiters of queued chunks complete normally; whatever cannot be
// extracted any more is released with ErrTripletsBatcherStopped instead of
// blocking until the timeout, including a tracked enqueue after Stop. goleak
// (leak_test.go) checks no goroutine stays behind.
func TestTripletsWait_StopUnblocksWaiters(t *testing.T) {
	store := newLockedTripletStore()
	ext := newGatedExtractor()
	b := NewChunkTripletsBatcher(&streamingBatchGen{}, store, ChunkTripletsBatcherConfig{
		QueueSize: 64, BatchSize: 2, MaxIdle: time.Hour, LLMTimeout: 5 * time.Second,
	})
	b.UseExtractor(ext)
	w1 := b.EnqueueTracked(chunkDocs("p", 2))
	w2 := b.EnqueueTracked(chunkDocs("q", 2))
	b.Start(context.Background())

	stopped := make(chan struct{})
	go func() { b.Stop(); close(stopped) }()
	releaserDone := make(chan struct{})
	go func() {
		defer close(releaserDone)
		for {
			select {
			case ext.gate <- struct{}{}:
			case <-stopped:
				return
			}
		}
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return")
	}
	<-releaserDone
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := w1.Wait(ctx); err != nil {
		t.Fatalf("w1: %v", err)
	}
	if err := w2.Wait(ctx); err != nil {
		t.Fatalf("w2 (backlog Stop drains): %v", err)
	}
	late := b.EnqueueTracked(chunkDocs("late", 1))
	if err := late.Wait(ctx); !errors.Is(err, ErrTripletsBatcherStopped) {
		t.Fatalf("enqueue after Stop = %v, want ErrTripletsBatcherStopped", err)
	}
}

// releaseRemaining (run as the drain loop exits) releases every chunk still
// queued or pending, so no waiter outlives the batcher.
func TestTripletsWait_ReleaseRemainingStopsQueuedWaiters(t *testing.T) {
	b := NewChunkTripletsBatcher(&streamingBatchGen{}, newLockedTripletStore(), ChunkTripletsBatcherConfig{
		QueueSize: 64, BatchSize: 2, MaxIdle: time.Hour, LLMTimeout: 5 * time.Second,
	})
	w := b.EnqueueTracked(chunkDocs("r", 3))
	b.drainQueueToPending() // as the exit path does: queued items become pending
	b.releaseRemaining()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := w.Wait(ctx); !errors.Is(err, ErrTripletsBatcherStopped) {
		t.Fatalf("Wait = %v, want ErrTripletsBatcherStopped", err)
	}
	if w.Failed() != 3 || w.Pending() != 0 {
		t.Fatalf("failed=%d pending=%d, want 3 and 0", w.Failed(), w.Pending())
	}
}

// A queue-full drop and a save error settle the chunk as failed rather than
// leaving the waiter blocked.
func TestTripletsWait_DropAndSaveErrorSettleAsFailed(t *testing.T) {
	store := newLockedTripletStore()
	store.fail["f1"] = true
	b := NewChunkTripletsBatcher(&streamingBatchGen{}, store, ChunkTripletsBatcherConfig{
		QueueSize: 2, BatchSize: 16, MaxIdle: time.Hour, LLMTimeout: 5 * time.Second,
	})
	b.UseExtractor(instantExtractor{})
	// Not started: queue of 2 holds f0 f1; f2 is dropped at enqueue.
	w := b.EnqueueTracked(chunkDocs("f", 3))
	if w.Failed() != 1 || w.Pending() != 2 {
		t.Fatalf("after a queue-full drop: failed=%d pending=%d, want 1 and 2", w.Failed(), w.Pending())
	}
	b.Start(context.Background())
	defer b.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := w.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if w.Failed() != 2 || !store.has("f0") {
		t.Fatalf("failed = %d (want 2: one dropped, one save error), f0 stored = %v", w.Failed(), store.has("f0"))
	}
}

// --- ProcessSync ---------------------------------------------------------

func newKGWaitIngestManager(t *testing.T, b *ChunkTripletsBatcher, wait bool, timeout time.Duration) *IngestionManager {
	t.Helper()
	im := newCaptureIngestManager(t, agentTimedParser{})
	im.cfg.SyncWaitForKG = wait
	if timeout > 0 {
		im.cfg.SyncKGWaitTimeout = timeout
	}
	im.SetChunkTripletsBatcher(b)
	return im
}

func kgDoc(uri string) domain.ExternalDocument {
	return domain.ExternalDocument{
		SourceURI: uri, SourceType: "ingest_memory", ThreadID: uri,
		Title: "s", Body: "user: I adopted a cat named Luna.\nassistant: Congratulations on Luna!",
	}
}

func stageSet(c *domain.StageClock) map[string]bool {
	got := map[string]bool{}
	for _, s := range c.Stages() {
		got[s.Stage] = true
	}
	return got
}

// Flag on: ProcessSync returns only after its chunks' triplets are persisted,
// and records kg_wait. The default timeout is 30 s.
func TestProcessSync_WaitForKG_ReturnsAfterTriplets(t *testing.T) {
	store := newLockedTripletStore()
	b := NewChunkTripletsBatcher(&streamingBatchGen{}, store, ChunkTripletsBatcherConfig{
		QueueSize: 64, BatchSize: 16, MaxIdle: time.Hour, LLMTimeout: 5 * time.Second,
	})
	b.UseExtractor(instantExtractor{})
	b.Start(context.Background())
	defer b.Stop()
	im := newKGWaitIngestManager(t, b, true, 0)
	if im.cfg.SyncKGWaitTimeout != 30*time.Second {
		t.Fatalf("default SyncKGWaitTimeout = %v, want 30s", im.cfg.SyncKGWaitTimeout)
	}
	clock := domain.NewStageClock()
	ctx := domain.WithStageClock(context.Background(), clock)
	if _, err := im.ProcessSync(ctx, kgDoc("ingest_memory://kg1")); err != nil {
		t.Fatalf("ProcessSync: %v", err)
	}
	if !stageSet(clock)[StageKGWait] {
		t.Fatalf("kg_wait stage not recorded: %v", clock.Stages())
	}
	enq, _, drained, _, _ := b.Stats()
	if enq == 0 || drained != enq {
		t.Fatalf("ProcessSync returned with %d of %d chunks extracted", drained, enq)
	}
	if c := clock.Counts(); c[CountKGPending] != 0 || c[CountKGFailed] != 0 {
		t.Fatalf("complete wait reported pending/failed: %v", c)
	}
}

// Timeout: the ingest still succeeds, logs a WARN, and reports kg_pending.
func TestProcessSync_WaitForKG_TimeoutSucceedsWithWarn(t *testing.T) {
	var buf bytes.Buffer
	var bufMu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lockedWriter{&buf, &bufMu}, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	store := newLockedTripletStore()
	ext := newGatedExtractor() // never released before the timeout
	b := NewChunkTripletsBatcher(&streamingBatchGen{}, store, ChunkTripletsBatcherConfig{
		QueueSize: 64, BatchSize: 16, MaxIdle: time.Hour, LLMTimeout: 5 * time.Second,
	})
	b.UseExtractor(ext)
	b.Start(context.Background())
	defer func() {
		go ext.release()
		b.Stop()
	}()
	im := newKGWaitIngestManager(t, b, true, 50*time.Millisecond)
	clock := domain.NewStageClock()
	ctx := domain.WithStageClock(context.Background(), clock)
	id, err := im.ProcessSync(ctx, kgDoc("ingest_memory://kg2"))
	if err != nil || id == "" {
		t.Fatalf("a KG timeout must not fail the ingest: id=%q err=%v", id, err)
	}
	if clock.Counts()[CountKGPending] < 1 {
		t.Fatalf("kg_pending not reported: %v", clock.Counts())
	}
	if line := FormatStageClock(clock, time.Millisecond); !strings.Contains(line, "kg_wait=") || !strings.Contains(line, "kg_pending#=") {
		t.Fatalf("trailer lacks kg_wait/kg_pending: %q", line)
	}
	bufMu.Lock()
	out := buf.String()
	bufMu.Unlock()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "reason=timeout") || !strings.Contains(out, "kg_pending=") {
		t.Fatalf("no timeout WARN logged: %q", out)
	}
}

type lockedWriter struct {
	b  *bytes.Buffer
	mu *sync.Mutex
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

// A cancelled caller ctx ends the wait; the ingest has already stored the
// content, so it is not failed by the KG lane.
func TestProcessSync_WaitForKG_CallerCancelEndsWait(t *testing.T) {
	store := newLockedTripletStore()
	ext := newGatedExtractor()
	b := NewChunkTripletsBatcher(&streamingBatchGen{}, store, ChunkTripletsBatcherConfig{
		QueueSize: 64, BatchSize: 16, MaxIdle: time.Hour, LLMTimeout: 5 * time.Second,
	})
	b.UseExtractor(ext)
	b.Start(context.Background())
	defer func() {
		go ext.release()
		b.Stop()
	}()
	im := newKGWaitIngestManager(t, b, true, time.Minute)
	clock := domain.NewStageClock()
	ctx, cancel := context.WithCancel(domain.WithStageClock(context.Background(), clock))
	go func() {
		// Cancel once the chunks are queued (the wait has begun).
		for {
			if e, _, _, _, _ := b.Stats(); e > 0 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	done := make(chan struct{})
	go func() {
		_, _ = im.ProcessSync(ctx, kgDoc("ingest_memory://kg3"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ProcessSync kept waiting after the caller cancelled (timeout is 1 minute)")
	}
	if clock.Counts()[CountKGPending] < 1 {
		t.Fatalf("kg_pending not reported on cancel: %v", clock.Counts())
	}
}

// Flag off: identical to before — untracked enqueue, no kg_wait stage, no
// change to batching (MaxIdle an hour: the chunks are still queued on return).
func TestProcessSync_WaitForKGOff_Unchanged(t *testing.T) {
	store := newLockedTripletStore()
	b := NewChunkTripletsBatcher(&streamingBatchGen{}, store, ChunkTripletsBatcherConfig{
		QueueSize: 64, BatchSize: 16, MaxIdle: time.Hour, LLMTimeout: 5 * time.Second,
	})
	b.UseExtractor(instantExtractor{})
	b.Start(context.Background())
	defer b.Stop()
	im := newKGWaitIngestManager(t, b, false, 0)
	clock := domain.NewStageClock()
	ctx := domain.WithStageClock(context.Background(), clock)
	if _, err := im.ProcessSync(ctx, kgDoc("ingest_memory://kg4")); err != nil {
		t.Fatalf("ProcessSync: %v", err)
	}
	if stageSet(clock)[StageKGWait] {
		t.Fatal("flag off recorded a kg_wait stage")
	}
	if !stageSet(clock)[StageTripletsEnqueue] {
		t.Fatal("flag off must still enqueue for async extraction")
	}
	time.Sleep(30 * time.Millisecond)
	if _, _, drained, _, _ := b.Stats(); drained != 0 {
		t.Fatalf("flag off drained %d chunks before a full batch or the idle timer: async batching changed", drained)
	}
}
