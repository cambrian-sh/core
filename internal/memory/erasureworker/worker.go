// Package erasureworker drains the async tail of ADR-0137 erasures (D7, S5; ENG-411
// stage B): the steps an erase transaction recorded in memory_erasure_outbox because
// they cannot run inside Postgres — today, deleting a content-addressed blob (evidence
// bytes, a source-document body) from the kernel's bbolt content store.
//
// Nothing here can make erased content readable again: every principal-serving copy was
// deleted by the transaction. A crash between commit and drain loses nothing — the
// outbox row survives and the worker resumes on boot.
package erasureworker

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// Config is the worker's tuning (execution.erasure).
type Config struct {
	// Poll is the drain interval (> 0).
	Poll time.Duration
	// MaxAttempts bounds the retries of one step; a step at the bound stays pending and
	// its request reads overdue past its async deadline.
	MaxAttempts int
	// TombstoneRetention prunes tombstones older than this; 0 keeps them forever.
	TombstoneRetention time.Duration
	// Batch is the steps drained per pass (0 ⇒ 100).
	Batch int
}

// Worker is the ONE owner of the erasure outbox drain: started by Start(ctx), drained
// by Stop() (AGENTS.md §3.3).
type Worker struct {
	outbox domain.ErasureOutbox
	cas    domain.ContentEraser
	cfg    Config
	logger *slog.Logger
	now    func() time.Time

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// New builds the worker. cas may be nil (no content store): cas_delete steps then stay
// pending, never silently marked done.
func New(outbox domain.ErasureOutbox, cas domain.ContentEraser, cfg Config, logger *slog.Logger) *Worker {
	if outbox == nil {
		panic("erasureworker.New: nil outbox")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.Batch <= 0 {
		cfg.Batch = 100
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 5
	}
	return &Worker{outbox: outbox, cas: cas, cfg: cfg, logger: logger, now: time.Now}
}

// Stats is what one pass did.
type Stats struct {
	Done, Shared, Failed, Completed, Pruned int
}

// RunOnce drains one batch, completes requests whose steps are all done and prunes
// tombstones past retention. Errors are logged; one failing step does not stop the
// others.
func (w *Worker) RunOnce(ctx context.Context) Stats {
	var st Stats
	rows, err := w.outbox.PendingSteps(ctx, w.cfg.Batch, w.cfg.MaxAttempts)
	if err != nil {
		w.logger.WarnContext(ctx, "erasure worker: reading the outbox failed", "err", err)
		return st
	}
	for _, r := range rows {
		if ctx.Err() != nil {
			break
		}
		shared, errClass := w.run(ctx, r)
		// The step's effect has happened (or definitively failed); record it even when
		// Stop cancels the pass meanwhile, keeping ctx's values (§2.7).
		mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		if errClass != "" {
			st.Failed++
			if err := w.outbox.MarkStepFailed(mctx, r.ID, errClass); err != nil {
				w.logger.WarnContext(ctx, "erasure worker: recording a failed step failed", "id", r.ID, "err", err)
			}
			w.logger.WarnContext(ctx, "erasure worker: step failed",
				"request_id", r.RequestID, "step", r.Step, "attempt", r.Attempts+1,
				"max_attempts", w.cfg.MaxAttempts, "error_class", errClass)
		} else {
			if _, err := w.outbox.MarkStepDone(mctx, r.ID); err != nil {
				w.logger.WarnContext(ctx, "erasure worker: marking a step done failed", "id", r.ID, "err", err)
			} else if shared {
				st.Shared++
			} else {
				st.Done++
			}
		}
		cancel()
	}
	if n, err := w.outbox.CompleteRequests(ctx); err != nil {
		w.logger.WarnContext(ctx, "erasure worker: completing requests failed", "err", err)
	} else {
		st.Completed = n
	}
	if w.cfg.TombstoneRetention > 0 {
		if n, err := w.outbox.PruneTombstones(ctx, w.now().Add(-w.cfg.TombstoneRetention)); err != nil {
			w.logger.WarnContext(ctx, "erasure worker: pruning tombstones failed", "err", err)
		} else {
			st.Pruned = n
		}
	}
	if st.Done+st.Shared+st.Failed+st.Completed+st.Pruned > 0 {
		w.logger.InfoContext(ctx, "erasure worker: pass",
			"done", st.Done, "kept_shared", st.Shared, "failed", st.Failed,
			"requests_completed", st.Completed, "tombstones_pruned", st.Pruned)
	}
	return st
}

// run executes one step. shared reports a blob kept because a surviving row still
// references it; errClass is "" on success, else a content-free error class.
func (w *Worker) run(ctx context.Context, r domain.ErasureOutboxRow) (shared bool, errClass string) {
	switch r.Step {
	case domain.ErasureStepCASDelete:
		if w.cas == nil {
			return false, "no_content_store"
		}
		inUse, err := w.outbox.CIDStillReferenced(ctx, r.Ref)
		if err != nil {
			return false, "refcount_failed"
		}
		if inUse {
			// Content-addressed: another surviving document or evidence holds the same
			// bytes. The erased record no longer points at it; the blob stays for the
			// record that does.
			return true, ""
		}
		if err := w.cas.DeleteContent(ctx, domain.CID(r.Ref)); err != nil {
			return false, "cas_delete_failed"
		}
		return false, ""
	default:
		return false, "unknown_step"
	}
}

// Start runs the worker until Stop: one pass at once (resume after a restart), then
// every Poll. A non-positive Poll ⇒ no goroutine. Idempotent.
func (w *Worker) Start(ctx context.Context) {
	if w.cfg.Poll <= 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cancel != nil {
		return
	}
	rctx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	done := make(chan struct{})
	w.done = done
	go func() {
		defer close(done)
		w.RunOnce(rctx)
		t := time.NewTicker(w.cfg.Poll)
		defer t.Stop()
		for {
			select {
			case <-rctx.Done():
				return
			case <-t.C:
				w.RunOnce(rctx)
			}
		}
	}()
}

// Stop ends the worker and waits for its pass to finish.
func (w *Worker) Stop() {
	w.mu.Lock()
	cancel, done := w.cancel, w.done
	w.cancel, w.done = nil, nil
	w.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}
