package evidence

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// The outbox's failure semantics (owner decision 2026-11-12; ADR-0105 D3 /
// ADR-0108 D3 / ADR-0112 amendment; T-20261112-evidence-outbox-is-head-of-line-
// blocked-the): not-ready items are HELD (no attempt, out of the pending read,
// back on release); failed items get EvidenceOutboxMaxAttempts tries with
// jittered exponential backoff, then a terminal dead state.

// exactJitter makes the backoff its upper bound for the test, so delays are
// exact rather than statistical.
func exactJitter(t *testing.T) {
	t.Helper()
	prev := outboxJitter
	outboxJitter = func(d time.Duration) time.Duration { return d }
	t.Cleanup(func() { outboxJitter = prev })
}

// routingTransformer answers per source key: "paused-*" not ready (hold key
// "ingress:paused"), "poison-*" a permanent error, "flaky-*" a transient error
// until healed, anything else handled.
type routingTransformer struct {
	paused bool
	healed bool
	seen   map[string]int
}

func (r *routingTransformer) Name() string { return "router" }
func (r *routingTransformer) Transform(_ context.Context, ev domain.Evidence, _ []byte) (bool, error) {
	if r.seen == nil {
		r.seen = map[string]int{}
	}
	r.seen[ev.SourceKey]++
	switch {
	case strings.HasPrefix(ev.SourceKey, "paused-") && r.paused:
		return true, fmt.Errorf("ingress-studio: wrapped: %w",
			domain.EvidenceNotReady("ingress:paused", "ingress paused"))
	case strings.HasPrefix(ev.SourceKey, "poison-"):
		return true, errors.New("permanently broken")
	case strings.HasPrefix(ev.SourceKey, "flaky-") && !r.healed:
		return true, errors.New("sink briefly down")
	}
	return true, nil
}

func retryFixture(t *testing.T, tr domain.EvidenceTransformer, keys ...string) (*Consumer, *fakeStore, *bytes.Buffer) {
	t.Helper()
	blobs, store := newFakeBlobs(), newFakeStore()
	store.clock = time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	ing := mustIngestor(t, blobs, store)
	for _, k := range keys {
		if _, _, err := ing.Ingest(context.Background(), raw(k)); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}
	var logs bytes.Buffer
	c, err := NewConsumer(store, blobs, []domain.EvidenceTransformer{tr},
		slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatal(err)
	}
	return c, store, &logs
}

func idOf(t *testing.T, store *fakeStore, key string) int64 {
	t.Helper()
	for _, it := range store.outbox {
		ev, _ := store.Get(context.Background(), it.EvidenceID)
		if ev != nil && ev.SourceKey == key {
			return it.ID
		}
	}
	t.Fatalf("no outbox item for %s", key)
	return 0
}

// The reproduced stall: 64+ items from a paused ingress at the head, one good
// item behind them. The good item is processed on the FIRST drain; the paused
// items consume no attempts and are not re-read on later ticks; resume
// (ReleaseHeldOutbox) drains them.
func TestConsumer_PausedHeadDoesNotBlockTheGoodItem(t *testing.T) {
	keys := make([]string, 0, 70)
	for i := range 70 {
		keys = append(keys, fmt.Sprintf("paused-%02d", i))
	}
	keys = append(keys, "good")
	tr := &routingTransformer{paused: true}
	c, store, _ := retryFixture(t, tr, keys...)

	c.drainUntilIdle(context.Background())

	if !store.processed[idOf(t, store, "good")] {
		t.Fatal("the good item behind 70 paused items was not processed on the first drain")
	}
	for i := range 70 {
		id := idOf(t, store, fmt.Sprintf("paused-%02d", i))
		if store.processed[id] || store.attempts[id] != 0 || store.held[id] != "ingress:paused" {
			t.Fatalf("paused item %d: processed=%v attempts=%d held=%q — want unprocessed, 0 attempts, held",
				i, store.processed[id], store.attempts[id], store.held[id])
		}
	}
	// Held items are out of the pending read: later ticks do not re-read them.
	before := tr.seen["paused-00"]
	c.drainUntilIdle(context.Background())
	c.drainUntilIdle(context.Background())
	if tr.seen["paused-00"] != before {
		t.Fatalf("a held item was re-delivered while still paused (%d → %d)", before, tr.seen["paused-00"])
	}

	// Resume: the ingress is armed again and its hold released.
	tr.paused = false
	n, err := store.ReleaseHeldOutbox(context.Background(), "ingress:paused")
	if err != nil || n != 70 {
		t.Fatalf("release = %d, %v; want 70", n, err)
	}
	c.drainUntilIdle(context.Background())
	for i := range 70 {
		id := idOf(t, store, fmt.Sprintf("paused-%02d", i))
		if !store.processed[id] || store.attempts[id] != 0 {
			t.Fatalf("paused item %d after resume: processed=%v attempts=%d", i, store.processed[id], store.attempts[id])
		}
	}
}

// A hold that was never released (a release that raced the hold, or an ingress
// retired while paused) comes back on its own after the recheck interval —
// still without consuming an attempt.
func TestConsumer_UnreleasedHoldIsRecheckedWithoutAnAttempt(t *testing.T) {
	tr := &routingTransformer{paused: true}
	c, store, _ := retryFixture(t, tr, "paused-a")
	c.drainUntilIdle(context.Background())
	id := idOf(t, store, "paused-a")
	if store.held[id] == "" {
		t.Fatal("not held")
	}
	store.clock = store.clock.Add(domain.EvidenceOutboxHoldRecheck)
	c.drainUntilIdle(context.Background())
	if tr.seen["paused-a"] != 2 || store.attempts[id] != 0 || store.held[id] == "" {
		t.Fatalf("recheck: seen=%d attempts=%d held=%q", tr.seen["paused-a"], store.attempts[id], store.held[id])
	}
	tr.paused = false
	store.clock = store.clock.Add(domain.EvidenceOutboxHoldRecheck)
	c.drainUntilIdle(context.Background())
	if !store.processed[id] {
		t.Fatal("a rechecked item whose ingress is ready again was not processed")
	}
}

// A permanently failing item: attempts with exponential backoff, dead after
// the 5th, listed by DeadOutbox, logged at error level, never retried again —
// and the items behind it are never blocked.
func TestConsumer_PoisonItemDiesAfterMaxAttempts(t *testing.T) {
	exactJitter(t)
	tr := &routingTransformer{}
	c, store, logs := retryFixture(t, tr, "poison-1", "good")
	id := idOf(t, store, "poison-1")

	c.drainUntilIdle(context.Background())
	if !store.processed[idOf(t, store, "good")] {
		t.Fatal("the good item behind a poison item was not processed on the first drain")
	}
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second}
	for attempt := 1; attempt < domain.EvidenceOutboxMaxAttempts; attempt++ {
		wait := store.due[id].Sub(store.clock)
		if store.attempts[id] != attempt || wait != want[attempt-1] {
			t.Fatalf("after attempt %d: attempts=%d backoff=%s, want %d and %s",
				attempt, store.attempts[id], wait, attempt, want[attempt-1])
		}
		// Not due before its backoff.
		c.drainUntilIdle(context.Background())
		if tr.seen["poison-1"] != attempt {
			t.Fatalf("retried before its backoff passed (attempt %d)", attempt)
		}
		store.clock = store.clock.Add(wait)
		c.drainUntilIdle(context.Background())
	}
	if _, dead := store.dead[id]; !dead || store.attempts[id] != domain.EvidenceOutboxMaxAttempts {
		t.Fatalf("after %d attempts: dead=%v attempts=%d", domain.EvidenceOutboxMaxAttempts, dead, store.attempts[id])
	}
	deadItems, _ := store.DeadOutbox(context.Background(), "", 10)
	if len(deadItems) != 1 || deadItems[0].ID != id || deadItems[0].LastError != "permanently broken" {
		t.Fatalf("DeadOutbox = %+v", deadItems)
	}
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "DEAD") {
		t.Fatalf("death was not logged at error level:\n%s", logs.String())
	}
	// Dead is terminal: no automatic retry, however long we wait.
	store.clock = store.clock.Add(24 * time.Hour)
	c.drainUntilIdle(context.Background())
	if tr.seen["poison-1"] != domain.EvidenceOutboxMaxAttempts {
		t.Fatalf("a dead item was retried: seen %d times", tr.seen["poison-1"])
	}
}

// A transient failure recovers within its attempts and leaves nothing dead.
func TestConsumer_TransientFailureRecovers(t *testing.T) {
	exactJitter(t)
	tr := &routingTransformer{}
	c, store, _ := retryFixture(t, tr, "flaky-1")
	id := idOf(t, store, "flaky-1")
	c.drainUntilIdle(context.Background())
	store.clock = store.clock.Add(domain.EvidenceOutboxBaseDelay)
	c.drainUntilIdle(context.Background()) // attempt 2 fails too
	tr.healed = true
	store.clock = store.clock.Add(2 * domain.EvidenceOutboxBaseDelay)
	c.drainUntilIdle(context.Background())
	if !store.processed[id] || store.attempts[id] != 2 {
		t.Fatalf("flaky item: processed=%v attempts=%d, want processed after 2 failed attempts", store.processed[id], store.attempts[id])
	}
	if _, dead := store.dead[id]; dead {
		t.Fatal("a recovered item is dead")
	}
}

// Backoff is the pipeline runtime's curve: BaseDelay*2^(n-1), capped.
func TestOutboxBackoff_MatchesThePipelineCurve(t *testing.T) {
	exactJitter(t)
	for attempt, want := range map[int]time.Duration{
		0: 500 * time.Millisecond, 1: 500 * time.Millisecond, 2: time.Second, 3: 2 * time.Second,
		4: 4 * time.Second, 10: 256 * time.Second, 11: 5 * time.Minute, 40: 5 * time.Minute,
	} {
		if got := outboxBackoff(attempt); got != want {
			t.Errorf("outboxBackoff(%d) = %s, want %s", attempt, got, want)
		}
	}
}

// A not-ready signal is recognised by TYPE through any wrapping, never by
// message text — and a plain error mentioning "paused" is a failure.
func TestConsumer_NotReadyIsTypedNotStringMatched(t *testing.T) {
	err := fmt.Errorf("outer: %w", domain.EvidenceNotReady("k", "paused"))
	if !errors.Is(err, domain.ErrEvidenceNotReady) {
		t.Fatal("wrapped NotReadyError is not ErrEvidenceNotReady")
	}
	tr := &fakeTransformer{name: "t", err: errors.New("ingress is paused; leaving the delivery pending")}
	c, _, store := consumerFixture(t, tr)
	c.drainOnce(context.Background())
	if store.attempts[1] != 1 || store.held[1] != "" {
		t.Fatalf("an untyped 'paused' error was treated as not-ready: attempts=%d held=%q", store.attempts[1], store.held[1])
	}
}
