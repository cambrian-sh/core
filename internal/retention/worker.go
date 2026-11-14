// Package retention prunes rows that nothing else ever deletes (ENG-128, owner
// decision 2026-11-15): operator audit after a fixed window, and opt-in TTLs on
// derived/cache tables. Evidence and knowledge are deliberately NOT here — they
// are the record, and deletion goes through the erasure path.
//
// The worker is table-agnostic: each Rule carries its own bounded purge function,
// so the Postgres specifics stay in internal/infrastructure/postgres and this
// package is unit-testable without a database.
package retention

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// PurgeFunc deletes up to limit rows older than before and reports how many it
// removed. Implementations must be bounded (one batch per call).
type PurgeFunc func(ctx context.Context, before time.Time, limit int) (int64, error)

// Rule is one table's retention policy.
type Rule struct {
	// Name labels the rule in logs (the table name).
	Name string
	// Keep is how long rows are kept. A rule with Keep <= 0 is skipped (keep forever).
	Keep  time.Duration
	Purge PurgeFunc
}

// maxBatchesPerPass caps the batches one rule may run in one pass, so a large
// backlog drains across passes rather than monopolising the database.
const maxBatchesPerPass = 20

// Worker runs every active rule on a fixed interval.
type Worker struct {
	rules    []Rule
	interval time.Duration
	batch    int
	logger   *slog.Logger
	now      func() time.Time

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// New builds a worker. Rules with Keep <= 0 or no Purge are dropped here, so
// Active reports what will actually run.
func New(rules []Rule, interval time.Duration, batch int, logger *slog.Logger) *Worker {
	if logger == nil {
		logger = slog.Default()
	}
	if batch <= 0 {
		batch = 5000
	}
	var active []Rule
	for _, r := range rules {
		if r.Keep > 0 && r.Purge != nil {
			active = append(active, r)
		}
	}
	return &Worker{rules: active, interval: interval, batch: batch, logger: logger, now: time.Now}
}

// Active returns the names of the rules that will run.
func (w *Worker) Active() []string {
	out := make([]string, 0, len(w.rules))
	for _, r := range w.rules {
		out = append(out, r.Name)
	}
	return out
}

// RunOnce applies every rule once and returns rows removed per rule. A rule's
// error is logged and does not stop the others.
func (w *Worker) RunOnce(ctx context.Context) map[string]int64 {
	removed := make(map[string]int64, len(w.rules))
	now := w.now().UTC()
	for _, r := range w.rules {
		before := now.Add(-r.Keep)
		for i := 0; i < maxBatchesPerPass; i++ {
			if ctx.Err() != nil {
				return removed
			}
			n, err := r.Purge(ctx, before, w.batch)
			if err != nil {
				w.logger.Warn("retention: purge failed", "table", r.Name, "err", err)
				break
			}
			removed[r.Name] += n
			if n < int64(w.batch) {
				break // drained
			}
		}
		if removed[r.Name] > 0 {
			w.logger.Info("retention: pruned rows past their retention",
				"table", r.Name, "removed", removed[r.Name], "before", before)
		}
	}
	return removed
}

// Start runs the worker until Stop. No active rules or a non-positive interval
// ⇒ no goroutine.
func (w *Worker) Start(ctx context.Context) {
	if len(w.rules) == 0 || w.interval <= 0 {
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
		t := time.NewTicker(w.interval)
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

// Stop ends the worker and waits for it.
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
