package memory

import (
	"context"
	"errors"
	"testing"
	"time"
)

// stopWithin fails the test when stop does not return within d: the defect
// these tests pin is a Stop that blocks forever.
func stopWithin(t *testing.T, d time.Duration, stop func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatal("Stop did not return")
	}
}

func newStopTestBatcher() *ChunkTripletsBatcher {
	return NewChunkTripletsBatcher(&streamingBatchGen{}, newLockedTripletStore(), ChunkTripletsBatcherConfig{
		QueueSize: 8, BatchSize: 2, MaxIdle: time.Hour, LLMTimeout: time.Second,
	})
}

// A batcher that was built but never started has no drain goroutine; Stop must
// return at once instead of waiting on a doneCh nothing will close
// (T-20261118-kernel-shutdown-hangs-forever-when-the-kernel), and twice.
func TestChunkTripletsBatcher_StopWithoutStart(t *testing.T) {
	b := newStopTestBatcher()
	stopWithin(t, 2*time.Second, b.Stop)
	stopWithin(t, 2*time.Second, b.Stop)
}

// Items tracked before a Stop-without-Start are released, not left to their
// timeout, and Start after Stop is a no-op (it launches no goroutine — goleak in
// TestMain checks — and the batcher stays closed).
func TestChunkTripletsBatcher_StopWithoutStartReleasesAndStartIsNoop(t *testing.T) {
	b := newStopTestBatcher()
	w := b.EnqueueTracked(context.Background(), chunkDocs("s", 2))
	stopWithin(t, 2*time.Second, b.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := w.Wait(ctx); !errors.Is(err, ErrTripletsBatcherStopped) {
		t.Fatalf("queued wait after Stop-without-Start = %v, want ErrTripletsBatcherStopped", err)
	}
	b.Start(context.Background())
	late := b.EnqueueTracked(context.Background(), chunkDocs("late", 1))
	if err := late.Wait(ctx); !errors.Is(err, ErrTripletsBatcherStopped) {
		t.Fatalf("enqueue after Stop then Start = %v, want ErrTripletsBatcherStopped", err)
	}
	stopWithin(t, 2*time.Second, b.Stop)
}

// Started then stopped twice: the second Stop returns too.
func TestChunkTripletsBatcher_StartedStopTwice(t *testing.T) {
	b := newStopTestBatcher()
	b.Start(context.Background())
	stopWithin(t, 2*time.Second, b.Stop)
	stopWithin(t, 2*time.Second, b.Stop)
}

// A nil batcher's Stop is a no-op.
func TestChunkTripletsBatcher_NilStop(t *testing.T) {
	var b *ChunkTripletsBatcher
	stopWithin(t, time.Second, b.Stop)
}
