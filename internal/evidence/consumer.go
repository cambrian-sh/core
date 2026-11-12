package evidence

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// Consumer drains the evidence outbox into the registered transformers — the
// transformation stage the outbox existed for (ADR-0108 D3; the ADR-0105
// deferral it closes).
//
// Guarantees, decomposed per ADR-0105 D3 and not summed into more:
//   - Delivery to transformers is AT LEAST ONCE (an error leaves the item
//     pending for the next poll).
//   - The outbox transition is exactly-once-logical (MarkProcessed's
//     conditional UPDATE), so two consumers racing one item do one transition.
//   - Transformers are replay-safe by construction: the typed stores they
//     write through are idempotent on source_ref.
//
// Failure handling (ADR-0105 D3 / ADR-0108 D3 / ADR-0112 amendment, owner
// decision 2026-11-12): a FAILED item gets domain.EvidenceOutboxMaxAttempts
// tries with full-jitter exponential backoff between them (the pipeline
// runtime's numbers), then is parked DEAD with its last error — logged at error
// level, listed by Ingress Studio, never retried automatically. A NOT-READY item
// (domain.ErrEvidenceNotReady, e.g. a paused ingress) is not a failure: it is
// HELD out of the pending read with no attempt consumed until its hold key is
// released. Either way the item leaves the due set, so a stuck head can never
// again stop the items behind it (T-20261112-evidence-outbox-is-head-of-line-
// blocked-the). Skipping past a stuck item is safe for correctness because
// "current" is chosen by event time, not processing order (ADR-0147).
type Consumer struct {
	store        domain.EvidenceStore
	blobs        domain.ContentStore
	transformers []domain.EvidenceTransformer
	interval     time.Duration
	batch        int
	logger       *slog.Logger
}

// NewConsumer builds an outbox consumer. It refuses to exist without at least
// one transformer: a consumer that consumes for nobody is the unwired-subsystem
// trap wearing a lifecycle.
func NewConsumer(store domain.EvidenceStore, blobs domain.ContentStore,
	transformers []domain.EvidenceTransformer, logger *slog.Logger) (*Consumer, error) {
	if store == nil || blobs == nil {
		return nil, errFmt("evidence consumer: store and content store are required")
	}
	if len(transformers) == 0 {
		return nil, errFmt("evidence consumer: no transformers registered — refusing to poll for nobody")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Consumer{
		store: store, blobs: blobs, transformers: transformers,
		interval: 2 * time.Second, batch: 64, logger: logger,
	}, nil
}

func errFmt(msg string) error { return &consumerError{msg} }

type consumerError struct{ msg string }

func (e *consumerError) Error() string { return e.msg }

// Run polls until ctx ends. One failed or not-ready item never blocks the rest
// of the batch, nor the items behind it.
func (c *Consumer) Run(ctx context.Context) {
	c.logger.Info("ADR-0108: evidence outbox consumer running",
		"transformers", len(c.transformers), "interval", c.interval)
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.drainUntilIdle(ctx)
		}
	}
}

// drainUntilIdle is the pipeline Drainer's busy-queue rule applied here: a
// pass that filled its batch and moved something goes straight round again.
// The ticker paces an IDLE queue; it must not cap catch-up throughput (one
// batch per tick was a hard ceiling of batch/interval items per second, felt as
// hours of lag after any downtime). Progress is required so a store outage — the
// one case where an item's state cannot change — waits out the tick instead of
// hot-looping. Holding or backing off an item IS progress: it leaves the due
// set, so the next read reaches the items behind it.
func (c *Consumer) drainUntilIdle(ctx context.Context) {
	for ctx.Err() == nil {
		fetched, moved := c.drainOnce(ctx)
		if fetched < c.batch || moved == 0 {
			return
		}
	}
}

// drainOnce processes one due batch, reporting how many items it fetched and
// how many left the due set (completed, held, backed off or dead).
func (c *Consumer) drainOnce(ctx context.Context) (fetched, moved int) {
	items, err := c.store.PendingOutbox(ctx, c.batch)
	if err != nil {
		c.logger.Warn("evidence consumer: pending read failed", "err", err)
		return 0, 0
	}
	for _, it := range items {
		if ctx.Err() != nil {
			return len(items), moved
		}
		perr := c.processItem(ctx, it)
		if perr == nil || c.settleFailure(ctx, it, perr) {
			moved++
		}
	}
	return len(items), moved
}

// settleFailure records what happened to an item whose processing did not
// complete, and reports whether its outbox state changed.
func (c *Consumer) settleFailure(ctx context.Context, it domain.EvidenceOutboxItem, perr error) bool {
	var notReady *domain.NotReadyError
	if errors.As(perr, &notReady) && notReady.HoldKey != "" {
		if err := c.store.HoldOutbox(ctx, it.ID, notReady.HoldKey, notReady.Reason, domain.EvidenceOutboxHoldRecheck); err != nil {
			c.logger.Warn("evidence consumer: could not hold a not-ready item; it stays due",
				"outbox_id", it.ID, "evidence_id", it.EvidenceID, "err", err)
			return false
		}
		c.logger.Debug("evidence consumer: item not ready, held",
			"outbox_id", it.ID, "evidence_id", it.EvidenceID, "hold", notReady.HoldKey)
		return true
	}
	attempts := it.Attempts + 1
	dead := attempts >= domain.EvidenceOutboxMaxAttempts
	var wait time.Duration
	if !dead {
		wait = outboxBackoff(attempts)
	}
	if err := c.store.RecordOutboxFailure(ctx, it.ID, attempts, perr.Error(), wait, dead); err != nil {
		c.logger.Warn("evidence consumer: could not record a failed attempt; the item stays due",
			"outbox_id", it.ID, "evidence_id", it.EvidenceID, "err", err, "cause", perr)
		return false
	}
	if dead {
		// Error level on purpose: a dead item is data that will not reach the
		// typed rows until a person acts; the archive is the only place it is.
		c.logger.Error("evidence consumer: item DEAD after exhausting its attempts — parked, not retried",
			"outbox_id", it.ID, "evidence_id", it.EvidenceID, "attempts", attempts, "err", perr)
		return true
	}
	// Due again after the backoff: at-least-once. Logged loudly because a
	// silently growing outbox is the failure mode the metrics caveat in the
	// ADR-0105 Known Gaps entry warned about.
	c.logger.Warn("evidence consumer: item failed; retrying after backoff",
		"outbox_id", it.ID, "evidence_id", it.EvidenceID, "attempt", attempts,
		"max_attempts", domain.EvidenceOutboxMaxAttempts, "retry_in", wait, "err", perr)
	return true
}

// outboxJitter is injectable so backoff tests are exact rather than statistical.
var outboxJitter = func(maxDelay time.Duration) time.Duration {
	if maxDelay <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(maxDelay) + 1))
}

// outboxBackoff is the delay after the given failed attempt (1-based):
// full-jitter exponential, delay in [0, min(MaxDelay, BaseDelay*2^(n-1))] — the
// formula of premium's pipeline.RetryPolicy.Backoff, mirrored (see
// domain.EvidenceOutboxMaxAttempts).
func outboxBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	capped := domain.EvidenceOutboxBaseDelay
	for range attempt - 1 {
		capped *= 2
		if capped >= domain.EvidenceOutboxMaxDelay {
			capped = domain.EvidenceOutboxMaxDelay
			break
		}
	}
	return outboxJitter(capped)
}

func (c *Consumer) processItem(ctx context.Context, it domain.EvidenceOutboxItem) error {
	ev, err := c.store.Get(ctx, it.EvidenceID)
	if err != nil {
		return err
	}
	node, err := c.blobs.Get(ctx, ev.ContentHash)
	if err != nil {
		// The ordering contract makes this near-impossible (bytes are durable
		// before the row exists); if it happens anyway, the item must stay
		// pending and SCREAM — this is the dangling-reference state Phase 1
		// exists to prevent.
		c.logger.Error("evidence consumer: content unreachable for committed evidence",
			"evidence_id", ev.ID, "cid", ev.ContentHash, "err", err)
		return err
	}
	handledBy := 0
	for _, tr := range c.transformers {
		handled, terr := tr.Transform(ctx, *ev, node.Data)
		if terr != nil {
			// Retry the whole item — transformers are replay-safe. A not-ready
			// signal (domain.ErrEvidenceNotReady) from any one transformer holds
			// the whole item; settleFailure tells the two apart by type.
			return terr
		}
		if handled {
			handledBy++
		}
	}
	if handledBy == 0 {
		// No transformer recognises this evidence. That is a normal state (the
		// memory-ingest lane archives everything; transformers pick shapes), so
		// the item completes rather than clogging the queue forever.
		c.logger.Debug("evidence consumer: no transformer claimed item", "evidence_id", ev.ID)
	}
	done, err := c.store.MarkProcessed(ctx, it.ID)
	if err != nil {
		return err
	}
	if !done {
		c.logger.Debug("evidence consumer: item already processed by another consumer", "outbox_id", it.ID)
	}
	return nil
}
