package evidence

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

type fakeTransformer struct {
	name    string
	handled bool
	err     error
	seen    [][]byte
}

func (f *fakeTransformer) Name() string { return f.name }
func (f *fakeTransformer) Transform(_ context.Context, _ domain.Evidence, content []byte) (bool, error) {
	f.seen = append(f.seen, content)
	return f.handled, f.err
}

func consumerFixture(t *testing.T, tr domain.EvidenceTransformer) (*Consumer, *fakeBlobs, *fakeStore) {
	t.Helper()
	blobs, store := newFakeBlobs(), newFakeStore()
	ing := mustIngestor(t, blobs, store)
	if _, _, err := ing.Ingest(context.Background(), raw("k1")); err != nil {
		t.Fatalf("seed ingest: %v", err)
	}
	c, err := NewConsumer(store, blobs, []domain.EvidenceTransformer{tr}, nil)
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}
	return c, blobs, store
}

// Gate: a handled item transitions exactly once and the transformer received
// the ORIGINAL bytes — the archive feeding the transformation stage.
func TestConsumer_DeliversBytesAndCompletesItem(t *testing.T) {
	tr := &fakeTransformer{name: "t", handled: true}
	c, _, store := consumerFixture(t, tr)
	c.drainOnce(context.Background())

	if len(tr.seen) != 1 || string(tr.seen[0]) != "delivery body for k1" {
		t.Fatalf("transformer did not receive the archived bytes: %q", tr.seen)
	}
	pending, _ := store.PendingOutbox(context.Background(), 10)
	if len(pending) != 0 {
		t.Fatalf("handled item still pending: %v", pending)
	}
}

// Gate (at-least-once): a transformer error leaves the item unprocessed with
// one attempt recorded and due again after its backoff, and the retry after
// recovery completes it.
func TestConsumer_ErrorLeavesItemPendingForRetry(t *testing.T) {
	exactJitter(t)
	tr := &fakeTransformer{name: "t", err: errors.New("injected")}
	c, _, store := consumerFixture(t, tr)
	store.clock = time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	c.drainOnce(context.Background())

	if store.processed[1] || store.attempts[1] != 1 {
		t.Fatalf("failed item: processed=%v attempts=%d, want unprocessed with 1 attempt", store.processed[1], store.attempts[1])
	}
	if pending, _ := store.PendingOutbox(context.Background(), 10); len(pending) != 0 {
		t.Fatal("a failed item must wait out its backoff, not be due again at once")
	}
	store.clock = store.clock.Add(domain.EvidenceOutboxBaseDelay)
	tr.err = nil
	tr.handled = true
	c.drainOnce(context.Background())
	if pending, _ := store.PendingOutbox(context.Background(), 10); len(pending) != 0 || !store.processed[1] {
		t.Fatal("retry after recovery did not complete the item")
	}
	if len(tr.seen) != 2 {
		t.Fatalf("expected exactly one redelivery, saw %d deliveries", len(tr.seen))
	}
}

// "Not mine" completes the item: an unclaimed shape must not clog the queue.
func TestConsumer_UnclaimedShapeCompletes(t *testing.T) {
	tr := &fakeTransformer{name: "t", handled: false}
	c, _, store := consumerFixture(t, tr)
	c.drainOnce(context.Background())
	pending, _ := store.PendingOutbox(context.Background(), 10)
	if len(pending) != 0 {
		t.Fatal("unclaimed item should complete, not clog the queue")
	}
}

// Busy-queue rule: a backlog deeper than one batch drains in ONE wake-up, not
// one batch per tick — the interval paces an idle queue, never catch-up.
func TestConsumer_DrainUntilIdleClearsABacklogDeeperThanOneBatch(t *testing.T) {
	tr := &fakeTransformer{name: "t", handled: true}
	c, blobs, store := consumerFixture(t, tr)
	ing := mustIngestor(t, blobs, store)
	for _, k := range []string{"k2", "k3", "k4"} {
		if _, _, err := ing.Ingest(context.Background(), raw(k)); err != nil {
			t.Fatalf("seed ingest %s: %v", k, err)
		}
	}
	c.batch = 2 // 4 pending items, so one batch cannot cover them

	c.drainUntilIdle(context.Background())

	pending, _ := store.PendingOutbox(context.Background(), 10)
	if len(pending) != 0 {
		t.Fatalf("drain-until-idle left %d items for a later tick", len(pending))
	}
}

// The progress guard: a failing item leaves the due set (backoff), so the
// drain returns to the ticker instead of hot-looping on it within one wake-up.
func TestConsumer_DrainUntilIdleYieldsWhenNothingCompletes(t *testing.T) {
	exactJitter(t)
	tr := &fakeTransformer{name: "t", err: errors.New("stuck")}
	c, _, store := consumerFixture(t, tr)
	store.clock = time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	c.batch = 1 // the failing item alone fills the batch

	c.drainUntilIdle(context.Background()) // must return, not spin

	if store.processed[1] || store.attempts[1] != 1 {
		t.Fatalf("the stuck item must stay unprocessed with one attempt: processed=%v attempts=%d",
			store.processed[1], store.attempts[1])
	}
	if len(tr.seen) != 1 {
		t.Fatalf("a failing pass must not retry within the same wake-up; saw %d deliveries", len(tr.seen))
	}
}

func TestConsumer_RefusesToExistWithoutTransformers(t *testing.T) {
	if _, err := NewConsumer(newFakeStore(), newFakeBlobs(), nil, nil); err == nil {
		t.Fatal("a consumer with no consumers must be a construction error")
	}
}

// principalTransformer records the principal a transformer runs under.
type principalTransformer struct{ seen domain.PrincipalRef }

func (p *principalTransformer) Name() string { return "principal" }
func (p *principalTransformer) Transform(ctx context.Context, _ domain.Evidence, _ []byte) (bool, error) {
	p.seen = domain.PrincipalFromContext(ctx)
	return true, nil
}

// ENG-494/ENG-449: a transformer's derived writes are made as the evidence's own
// source service principal (source:<connector>:<id> from Evidence.SourceID), never
// as no one.
func TestConsumer_TransformersRunAsTheEvidenceSource(t *testing.T) {
	tr := &principalTransformer{}
	c, _, _ := consumerFixture(t, tr)
	c.drainOnce(context.Background())
	if tr.seen.String() != "source:test:src" {
		t.Fatalf("transformer principal %v, want source:test:src", tr.seen)
	}
}
