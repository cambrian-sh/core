package memory

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/cambrian-sh/core/domain"
)

// ErrTripletsBatcherStopped is returned by TripletsWait.Wait when the batcher
// stopped before every tracked chunk was extracted. The chunks themselves are
// stored; only their KG enrichment is missing.
var ErrTripletsBatcherStopped = errors.New("chunk triplets batcher stopped before extraction completed")

// tripletItem is one queued chunk. wait is nil for an untracked (async) enqueue.
type tripletItem struct {
	doc  *domain.Document
	wait *TripletsWait
	// attempts counts how many times this item was re-queued after its batch
	// failed because the extractor was unavailable (bounded by maxRequeues).
	attempts int
}

// TripletsWait is the completion signal for one document's chunks in the
// ChunkTripletsBatcher (execution.ingestion.sync_ingest_wait_for_kg). The
// batcher settles each tracked chunk exactly once; when the last one settles,
// done closes. It holds no goroutine: the drain loop that already exists does
// the signalling, so waiting adds no goroutine per ingest (AGENTS.md §3.3).
//
// All methods are nil-safe, so the untracked path (wait == nil) needs no branch.
type TripletsWait struct {
	remaining atomic.Int64
	failed    atomic.Int64 // chunks that settled without their triplets persisted
	stopped   atomic.Bool  // the batcher stopped before every chunk settled
	done      chan struct{}
	once      sync.Once
}

func newTripletsWait(n int) *TripletsWait {
	w := &TripletsWait{done: make(chan struct{})}
	w.remaining.Store(int64(n))
	if n <= 0 {
		w.once.Do(func() { close(w.done) })
	}
	return w
}

// settle marks n chunks finished. ok=false means their triplets were NOT
// persisted (extraction failed, dropped on a full queue, or a save error).
func (w *TripletsWait) settle(n int, ok bool) {
	if w == nil || n <= 0 {
		return
	}
	if !ok {
		w.failed.Add(int64(n))
	}
	if w.remaining.Add(-int64(n)) <= 0 {
		w.once.Do(func() { close(w.done) })
	}
}

// stop settles n chunks as abandoned because the batcher is shutting down.
func (w *TripletsWait) stop(n int) {
	if w == nil {
		return
	}
	w.stopped.Store(true)
	w.settle(n, false)
}

// Wait blocks until every tracked chunk has settled or ctx is done. It returns
// nil when all chunks settled (some may still have failed — see Failed),
// ErrTripletsBatcherStopped when the batcher shut down first, and ctx.Err() on
// cancellation or deadline. Cancelling ctx stops the WAIT only: the chunks stay
// queued and are extracted as usual.
func (w *TripletsWait) Wait(ctx context.Context) error {
	if w == nil {
		return nil
	}
	select {
	case <-w.done:
		if w.stopped.Load() {
			return ErrTripletsBatcherStopped
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Pending is the number of tracked chunks not yet settled.
func (w *TripletsWait) Pending() int {
	if w == nil {
		return 0
	}
	if r := w.remaining.Load(); r > 0 {
		return int(r)
	}
	return 0
}

// Failed is the number of tracked chunks that settled without their triplets
// persisted (extraction error after any re-queues, queue full, save error, or
// batcher stopped).
func (w *TripletsWait) Failed() int {
	if w == nil {
		return 0
	}
	return int(w.failed.Load())
}
