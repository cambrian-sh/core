package evidence

import (
	"context"
	"log/slog"
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

// Run polls until ctx ends. One failed item never blocks the rest of the batch.
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
			c.drainOnce(ctx)
		}
	}
}

// drainOnce processes one pending batch. Exported-ish via tests; the loop is
// just this on a ticker.
func (c *Consumer) drainOnce(ctx context.Context) {
	items, err := c.store.PendingOutbox(ctx, c.batch)
	if err != nil {
		c.logger.Warn("evidence consumer: pending read failed", "err", err)
		return
	}
	for _, it := range items {
		if err := c.processItem(ctx, it); err != nil {
			// Leave pending: at-least-once. Logged loudly because a silently
			// growing outbox is the failure mode the metrics caveat in the
			// ADR-0105 Known Gaps entry warned about.
			c.logger.Warn("evidence consumer: item left pending for retry",
				"outbox_id", it.ID, "evidence_id", it.EvidenceID, "err", err)
		}
	}
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
			return terr // pending; retry the whole item — transformers are replay-safe
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
