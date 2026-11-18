package memory

import (
	"context"
	"log/slog"
	"sort"

	"github.com/cambrian-sh/core/domain"
)

// ENG-2111: fair, resumable feeding of journaled KG triplet jobs.
//
// Before, a journaled document's chunks all went into the ChunkTripletsBatcher queue
// at once. One large ingest (a 5 MB text: 5295 chunks against a 4096-slot queue) then
// filled the queue: its own tail chunks and the NEXT, unrelated documents met a full
// queue (readiness run ENG-1995 B3). ENG-2122 turned those drops into deferrals, but
// two defects remained: every other document waited behind the big one, and a document
// with more chunks than the queue holds could never finish — each replay re-enqueued
// the whole document, overflowed again, and was deferred again, forever.
//
// Now the derivation journal feeds a KG job to the batcher in SLICES of at most
// KGSliceChunks chunks (never more than the queue holds), and keeps at most
// KGSlicesInFlight slices of one document queued at a time. A document with more to
// feed waits in a round-robin ring, so the slices of all documents take turns: a small
// document ingested during a large one waits behind at most KGSlicesInFlight slices of
// it, not behind the whole document. A slice is fed only when the batcher has room for
// it (backpressure in memory instead of a queue-full drop), and after every finished
// slice the journal row shrinks to the chunks still to derive (AdvanceDerivation), so a
// restart resumes where the previous process stopped.
//
// This is the same pathway as before (AGENTS.md §4.3): the same batcher, the same
// completion semantics per document (done → row deleted; failed → retried with backoff;
// deferred → retried after RetryBase; released → left for the next process), and the
// same replay context and version fence. Only the admission order changed.
//
// Threads. Slice outcomes arrive on the batcher's drain goroutine (and inside enqueue
// for a deferral); they update the feed under feedMu and write the journal row, but
// never enqueue — feeding happens on the journaler's loop goroutine (kick), on the
// ingest goroutine for a new document's first slice, and in RecoverOnce.
//
// Claims. A feed waiting in the ring holds its row's claim; a claim older than Lease
// may be taken by ANOTHER process's recovery loop (this process's in-flight set skips
// it), which then derives the same chunks again — idempotent rewrites, never a loss.

// kgFeed is one journaled document's KG job being fed slice by slice. Guarded by the
// journaler's feedMu.
type kgFeed struct {
	job domain.DerivationJob
	// ctx is the replay context (journalReplayCtx): the maintenance read scope for the
	// slices read back from the store, and the version and ingest origin the batcher
	// fences each triplet save with.
	ctx context.Context
	// hookCtx is the context the journal writes log under.
	hookCtx context.Context
	// pending are the chunk ids not yet fed, in order; inflight the fed slices not yet
	// settled, by slice number; retry the chunks of slices that ended without success.
	pending  []string
	inflight map[int][]string
	retry    []string
	next     int
	// worst is the most significant non-done slice outcome (released over failed over
	// deferred), err its cause.
	worst derivationOutcome
	err   error
	// halted: a slice ended without success, or the journaler stopped — feed no more.
	halted bool
	// advanced: a slice finished since the row's progress was last written.
	advanced bool
	inRing   bool
	finished bool
	// wait is the sync ingest's TripletsWait across every slice (nil when untracked).
	wait *TripletsWait
}

// journalReplayCtx is the context a journaled job is fed under — see the
// DerivationJournaler type comment (Access): a replay serves no principal, reads under
// the maintenance scope and carries the journaled version and ingest origin.
func journalReplayCtx(ctx context.Context, j domain.DerivationJob) context.Context {
	rctx := domain.WithScope(context.WithoutCancel(ctx), domain.ScopeMaintenance)
	rctx = domain.WithPrincipal(rctx, domain.SystemPrincipal)
	rctx = domain.WithSourceVersion(rctx, domain.SourceVersion{DocumentID: j.DocumentID, ContentDigest: j.ContentDigest})
	return domain.WithIngestOrigin(rctx, j.IngestOrigin)
}

func (d *DerivationJournaler) newFeed(ctx context.Context, j domain.DerivationJob, w *TripletsWait) *kgFeed {
	return &kgFeed{
		job: j, ctx: journalReplayCtx(ctx, j), hookCtx: context.WithoutCancel(ctx),
		pending:  append([]string(nil), j.ChunkIDs...),
		inflight: map[int][]string{}, wait: w,
	}
}

// sliceSize is the chunks one slice holds: KGSliceChunks, never more than the batcher
// queue holds (a slice larger than the queue could never be admitted).
func (d *DerivationJournaler) sliceSize() int {
	n := d.cfg.KGSliceChunks
	if c := d.kg.capacity(); c > 0 && n > c {
		n = c
	}
	return max(n, 1)
}

// startKG feeds a live ingest's journaled KG job (ENG-2111). docs are the stored
// chunks in the job's order; the first slice is fed from them at once when no other
// document is waiting and the batcher has room, otherwise the document joins the ring
// and its slices are read back from the store as their turn comes. It returns the sync
// ingest's wait when track is set.
func (d *DerivationJournaler) startKG(ctx context.Context, j domain.DerivationJob, docs []*domain.Document, track bool) *TripletsWait {
	if d.halt.Load() {
		// Shutting down: no loop feeds later slices. Queue the document whole as before;
		// whatever the batcher cannot take is deferred, and the row stays for the next
		// process.
		return d.kg.enqueueDocument(journalReplayCtx(ctx, j), docs, track, d.hookFor(ctx, j))
	}
	var w *TripletsWait
	if track {
		w = newTripletsWait(len(j.ChunkIDs))
	}
	f := d.newFeed(ctx, j, w)
	d.feedMu.Lock()
	d.feeds[j.Key()] = f
	n := min(d.sliceSize(), len(f.pending))
	if len(d.ring) == 0 && d.roomLocked() >= n {
		sid, ids := d.takeSliceLocked(f)
		d.requeueLocked(f)
		d.feedMu.Unlock()
		var first []*domain.Document
		if len(docs) >= len(ids) {
			first = docs[:len(ids)]
			for i, doc := range first {
				if doc == nil || doc.ID != ids[i] {
					first = nil // not the journaled order: read the slice back instead
					break
				}
			}
		}
		d.feedSlice(f, sid, ids, first)
	} else {
		d.requeueLocked(f)
		d.feedMu.Unlock()
	}
	d.nudge()
	return w
}

// queueFeed puts a claimed (recovered) KG job in the ring. The caller pumps.
func (d *DerivationJournaler) queueFeed(ctx context.Context, j domain.DerivationJob) {
	f := d.newFeed(ctx, j, nil)
	d.feedMu.Lock()
	d.feeds[j.Key()] = f
	d.requeueLocked(f)
	empty := len(f.pending) == 0
	d.feedMu.Unlock()
	if empty {
		d.complete(ctx, j.Key()) // nothing left to derive
		d.feedMu.Lock()
		delete(d.feeds, j.Key())
		d.feedMu.Unlock()
	}
}

// activeFeeds is the KG jobs being fed in this process.
func (d *DerivationJournaler) activeFeeds() int {
	d.feedMu.Lock()
	defer d.feedMu.Unlock()
	return len(d.feeds)
}

// roomLocked is the batcher's free room minus the slices taken but not yet enqueued.
func (d *DerivationJournaler) roomLocked() int {
	return d.kg.headroom() - d.reserved
}

// takeSliceLocked moves the feed's next slice from pending to inflight and reserves
// its room until feedSlice enqueued it.
func (d *DerivationJournaler) takeSliceLocked(f *kgFeed) (int, []string) {
	n := min(d.sliceSize(), len(f.pending))
	ids := f.pending[:n:n]
	f.pending = f.pending[n:]
	sid := f.next
	f.next++
	f.inflight[sid] = ids
	d.reserved += n
	return sid, ids
}

// requeueLocked appends the feed to the ring tail when it has more to feed and a free
// in-flight slot.
func (d *DerivationJournaler) requeueLocked(f *kgFeed) {
	if f.halted || f.finished || f.inRing || len(f.pending) == 0 || len(f.inflight) >= d.cfg.KGSlicesInFlight {
		return
	}
	f.inRing = true
	d.ring = append(d.ring, f)
}

func (d *DerivationJournaler) dropFromRingLocked(f *kgFeed) {
	if !f.inRing {
		return
	}
	f.inRing = false
	for i, g := range d.ring {
		if g == f {
			d.ring = append(d.ring[:i:i], d.ring[i+1:]...)
			return
		}
	}
}

// nudge wakes the loop to feed the ring. Never blocks.
func (d *DerivationJournaler) nudge() {
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

// pump feeds the ring head's next slice while the batcher has room for it, round
// robin. It returns the slices fed. The ring head is never skipped for a smaller
// slice behind it, so a large document's slice cannot be starved by small ones.
func (d *DerivationJournaler) pump() int {
	if d == nil || d.kg == nil {
		return 0
	}
	fed := 0
	for {
		d.feedMu.Lock()
		if d.halt.Load() || len(d.ring) == 0 {
			d.feedMu.Unlock()
			return fed
		}
		f := d.ring[0]
		if n := min(d.sliceSize(), len(f.pending)); d.roomLocked() < n {
			d.feedMu.Unlock()
			return fed // backpressure: a settled slice nudges the loop again
		}
		d.ring[0] = nil
		d.ring = d.ring[1:]
		f.inRing = false
		sid, ids := d.takeSliceLocked(f)
		d.requeueLocked(f)
		d.feedMu.Unlock()
		d.feedSlice(f, sid, ids, nil)
		fed++
	}
}

// feedSlice enqueues one slice: from the chunks in hand (a live ingest's first slice)
// or read back through the enforcing read store under the replay scope.
func (d *DerivationJournaler) feedSlice(f *kgFeed, sid int, ids []string, chunks []*domain.Document) {
	defer func() {
		d.feedMu.Lock()
		d.reserved -= len(ids)
		d.feedMu.Unlock()
	}()
	if chunks == nil {
		got, err := readChunksInOrder(f.ctx, d.reader, ids)
		if err != nil {
			slog.WarnContext(f.hookCtx, "DerivationJournal: chunks of a KG slice unreadable; retried later",
				"document_id", f.job.DocumentID, "chunks", len(ids), "err", err)
			f.wait.settle(len(ids), false)
			d.settleSlice(f, sid, derivationDeferred, err)
			return
		}
		chunks = got
	}
	// Chunks gone since (erased, or a revision without them) have nothing to derive.
	f.wait.settle(len(ids)-len(chunks), true)
	if len(chunks) == 0 {
		d.settleSlice(f, sid, derivationDone, nil)
		return
	}
	d.kgSlices.Add(1)
	d.kg.enqueueSlice(f.ctx, chunks, f.wait, func(o derivationOutcome, err error) { d.settleSlice(f, sid, o, err) })
}

// settleSlice records one slice's outcome. When the feed is finished it settles the
// document's journal row; otherwise it writes the progress and lets the loop feed the
// next slice.
func (d *DerivationJournaler) settleSlice(f *kgFeed, sid int, o derivationOutcome, err error) {
	k := f.job.Key()
	d.feedMu.Lock()
	ids, ok := f.inflight[sid]
	if !ok || f.finished {
		d.feedMu.Unlock()
		return
	}
	delete(f.inflight, sid)
	if o == derivationDone {
		f.advanced = true
	} else {
		f.halted = true
		f.retry = append(f.retry, ids...)
		if o > f.worst || (o == f.worst && f.err == nil) {
			f.worst, f.err = o, err
		}
	}
	if d.halt.Load() {
		f.halted = true // the journaler stopped: no slice is fed any more
	}
	finished := len(f.inflight) == 0 && (f.halted || len(f.pending) == 0)
	var remaining []string
	advanced := f.advanced
	if advanced || finished {
		remaining = f.remainingLocked()
		f.advanced = false
	}
	unfed := 0
	if finished {
		f.finished = true
		unfed = len(f.pending)
		delete(d.feeds, k)
		d.dropFromRingLocked(f)
	} else {
		d.requeueLocked(f)
	}
	worst, cause := f.worst, f.err
	d.feedMu.Unlock()

	if !finished {
		if advanced {
			d.advance(f.hookCtx, k, remaining)
		}
		d.nudge()
		return
	}
	switch {
	case worst == derivationDone && unfed == 0 && len(f.retry) == 0:
		d.complete(f.hookCtx, k)
	case worst == derivationDone || worst == derivationReleased:
		// Stopped with chunks left: the row stays (claimed by this ending process) with
		// its progress, for the next process.
		if advanced {
			d.advance(f.hookCtx, k, remaining)
		}
		f.wait.stop(unfed)
		d.clearInflight(k)
	default:
		if advanced {
			d.advance(f.hookCtx, k, remaining)
		}
		f.wait.settle(unfed, false)
		d.reschedule(f.hookCtx, f.job, cause, worst == derivationFailed)
	}
	d.nudge() // the room this document held is free for the next one
}

// remainingLocked is the chunks the feed has not derived yet: failed slices, slices
// in flight (in slice order) and the chunks not yet fed.
func (f *kgFeed) remainingLocked() []string {
	out := append([]string{}, f.retry...)
	sids := make([]int, 0, len(f.inflight))
	for sid := range f.inflight {
		sids = append(sids, sid)
	}
	sort.Ints(sids)
	for _, sid := range sids {
		out = append(out, f.inflight[sid]...)
	}
	return append(out, f.pending...)
}

// advance writes a feed's progress. A failed write only costs re-extracting the
// finished chunks after a restart (idempotent rewrites).
func (d *DerivationJournaler) advance(ctx context.Context, k domain.DerivationKey, remaining []string) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.cfg.StoreTimeout)
	defer cancel()
	if err := d.journal.AdvanceDerivation(wctx, k, remaining); err != nil {
		slog.WarnContext(ctx, "DerivationJournal: KG progress not recorded; a replay re-extracts the finished chunks (idempotently)",
			"document_id", k.DocumentID, "remaining", len(remaining), "err", err)
		return
	}
	d.kgAdvanced.Add(1)
}

// releaseWaitingFeeds runs once the loop stopped: feeds with nothing in flight will
// never be fed in this process, so their rows stay for the next one (with the progress
// already written) and their sync waiters are released.
func (d *DerivationJournaler) releaseWaitingFeeds() {
	d.feedMu.Lock()
	var idle []*kgFeed
	for k, f := range d.feeds {
		f.halted = true
		if len(f.inflight) == 0 {
			f.finished = true
			delete(d.feeds, k)
			idle = append(idle, f)
		}
	}
	d.ring = nil
	d.feedMu.Unlock()
	for _, f := range idle {
		f.inRing = false
		f.wait.stop(len(f.pending))
		d.clearInflight(f.job.Key())
	}
}
