package evidence_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/evidence"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
)

// The outbox failure semantics against the REAL Postgres store and migration
// 0039 (PG_TEST_DSN; `orchestrator migrate up`): the reproduced stall (64+ items
// from a paused ingress ahead of one good item), holds that consume no attempt
// and release on resume, attempts with backoff to a dead state that DeadOutbox
// lists, and a transient failure that recovers. Owner decision 2026-11-12,
// T-20261112-evidence-outbox-is-head-of-line-blocked-the.

const pgOutboxSource = "outboxpg:"

type pgOutbox struct {
	pool     *pgxpool.Pool
	store    *postgres.PgEvidenceStore
	ingest   *evidence.Ingestor
	consumer *evidence.Consumer
	tr       *pgRouter
	ctx      context.Context
}

// pgRouter claims only this test's evidence (source id prefix): "paused" →
// not ready, "poison" → permanent error, "flaky" → error until healed.
type pgRouter struct {
	paused, healed bool
	seen           map[string]int
}

func (r *pgRouter) Name() string { return "outboxpg" }
func (r *pgRouter) Transform(_ context.Context, ev domain.Evidence, _ []byte) (bool, error) {
	if !strings.HasPrefix(ev.SourceID, pgOutboxSource) {
		return false, nil
	}
	r.seen[ev.SourceKey]++
	switch strings.TrimPrefix(ev.SourceID, pgOutboxSource) {
	case "paused":
		if r.paused {
			return true, fmt.Errorf("studio: %w", domain.EvidenceNotReady(ev.SourceID, "paused"))
		}
	case "poison":
		return true, errors.New("permanently broken")
	case "flaky":
		if !r.healed {
			return true, errors.New("briefly down")
		}
	}
	return true, nil
}

func newPgOutbox(t *testing.T) *pgOutbox {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping integration test that requires PostgreSQL")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	clean := func() {
		for _, q := range []string{
			`DELETE FROM evidence_outbox WHERE evidence_id IN (SELECT id FROM evidence WHERE source_id LIKE 'outboxpg:%')`,
			`DELETE FROM evidence WHERE source_id LIKE 'outboxpg:%'`,
		} {
			if _, err := pool.Exec(context.Background(), q); err != nil {
				t.Logf("cleanup %q: %v", q, err)
			}
		}
	}
	clean()
	t.Cleanup(clean)
	store := postgres.NewPgEvidenceStore(pool)
	blobs := evidence.NewTestBlobs()
	ing, err := evidence.NewIngestor(blobs, store)
	if err != nil {
		t.Fatal(err)
	}
	tr := &pgRouter{seen: map[string]int{}}
	c, err := evidence.NewConsumer(store, blobs, []domain.EvidenceTransformer{tr}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &pgOutbox{pool: pool, store: store, ingest: ing, consumer: c, tr: tr, ctx: ctx}
}

func (f *pgOutbox) put(t *testing.T, source, key string) {
	t.Helper()
	if _, _, err := f.ingest.Ingest(f.ctx, domain.RawEvidence{SourceID: pgOutboxSource + source, SourceKey: key,
		Bytes: []byte("body " + source + "/" + key)}); err != nil {
		t.Fatalf("ingest %s/%s: %v", source, key, err)
	}
}

type outboxRow struct {
	processed, dead bool
	attempts        int
	held, lastErr   string
	dueIn           time.Duration // next_attempt_at - now(); 0 when NULL
}

func (f *pgOutbox) row(t *testing.T, key string) outboxRow {
	t.Helper()
	var r outboxRow
	var due *float64
	if err := f.pool.QueryRow(f.ctx, `
		SELECT o.processed_at IS NOT NULL, o.dead_at IS NOT NULL, o.attempts, o.held_key, o.last_error,
		       EXTRACT(EPOCH FROM (o.next_attempt_at - now()))
		FROM evidence_outbox o JOIN evidence e ON e.id = o.evidence_id
		WHERE e.source_id LIKE 'outboxpg:%' AND e.source_key = $1`, key).
		Scan(&r.processed, &r.dead, &r.attempts, &r.held, &r.lastErr, &due); err != nil {
		t.Fatalf("outbox row %s: %v", key, err)
	}
	if due != nil {
		r.dueIn = time.Duration(*due * float64(time.Second))
	}
	return r
}

// makeDue moves an item's next attempt into the past — the DB clock cannot be
// advanced, so the test advances the row instead.
func (f *pgOutbox) makeDue(t *testing.T, key string) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx, `
		UPDATE evidence_outbox SET next_attempt_at = now() - interval '1 second'
		WHERE evidence_id IN (SELECT id FROM evidence WHERE source_id LIKE 'outboxpg:%' AND source_key = $1)`, key); err != nil {
		t.Fatal(err)
	}
}

func TestPgOutbox_PausedHeadDoesNotBlockTheGoodItem(t *testing.T) {
	f := newPgOutbox(t)
	f.tr.paused = true
	for i := range 70 {
		f.put(t, "paused", fmt.Sprintf("p-%02d", i))
	}
	f.put(t, "good", "g-1")

	evidence.DrainUntilIdle(f.consumer, f.ctx)

	if r := f.row(t, "g-1"); !r.processed {
		t.Fatalf("the good item behind 70 paused items was not processed on the first drain: %+v", r)
	}
	for i := range 70 {
		r := f.row(t, fmt.Sprintf("p-%02d", i))
		if r.processed || r.attempts != 0 || r.held != pgOutboxSource+"paused" || r.dead {
			t.Fatalf("paused item %d = %+v, want unprocessed, 0 attempts, held", i, r)
		}
		if r.dueIn < domain.EvidenceOutboxHoldRecheck-time.Minute {
			t.Fatalf("paused item %d is due again in %s, want ~%s", i, r.dueIn, domain.EvidenceOutboxHoldRecheck)
		}
	}
	// Held rows are not in the pending read.
	pending, err := f.store.PendingOutbox(f.ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range pending {
		ev, _ := f.store.Get(f.ctx, it.EvidenceID)
		if ev != nil && strings.HasPrefix(ev.SourceID, pgOutboxSource) {
			t.Fatalf("a held item is in the pending read: %+v", it)
		}
	}
	// Resume.
	f.tr.paused = false
	n, err := f.store.ReleaseHeldOutbox(f.ctx, pgOutboxSource+"paused")
	if err != nil || n != 70 {
		t.Fatalf("release = %d, %v; want 70", n, err)
	}
	evidence.DrainUntilIdle(f.consumer, f.ctx)
	for i := range 70 {
		if r := f.row(t, fmt.Sprintf("p-%02d", i)); !r.processed || r.attempts != 0 || r.held != "" {
			t.Fatalf("paused item %d after resume = %+v", i, r)
		}
	}
}

func TestPgOutbox_PoisonDiesAfterFiveAttemptsAndIsListed(t *testing.T) {
	f := newPgOutbox(t)
	evidence.ExactJitter(t)
	f.put(t, "poison", "x-1")
	f.put(t, "good", "g-2")

	evidence.DrainUntilIdle(f.consumer, f.ctx)
	if r := f.row(t, "g-2"); !r.processed {
		t.Fatal("the good item behind a poison item was not processed")
	}
	wantBackoff := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second}
	for attempt := 1; attempt < domain.EvidenceOutboxMaxAttempts; attempt++ {
		r := f.row(t, "x-1")
		if r.attempts != attempt || r.dead || r.processed || r.lastErr != "permanently broken" {
			t.Fatalf("after attempt %d: %+v", attempt, r)
		}
		// The backoff is the curve's value, by the DB clock (±250 ms for the round trip).
		if d := r.dueIn - wantBackoff[attempt-1]; d > 250*time.Millisecond || d < -250*time.Millisecond {
			t.Fatalf("after attempt %d: due in %s, want ~%s", attempt, r.dueIn, wantBackoff[attempt-1])
		}
		f.makeDue(t, "x-1")
		evidence.DrainUntilIdle(f.consumer, f.ctx)
	}
	r := f.row(t, "x-1")
	if !r.dead || r.attempts != domain.EvidenceOutboxMaxAttempts || r.processed {
		t.Fatalf("after %d attempts: %+v, want dead", domain.EvidenceOutboxMaxAttempts, r)
	}
	dead, err := f.store.DeadOutbox(f.ctx, pgOutboxSource+"poison", 10)
	if err != nil || len(dead) != 1 || dead[0].LastError != "permanently broken" ||
		dead[0].Attempts != domain.EvidenceOutboxMaxAttempts || dead[0].SourceID != pgOutboxSource+"poison" {
		t.Fatalf("DeadOutbox = %+v, %v", dead, err)
	}
	if other, _ := f.store.DeadOutbox(f.ctx, pgOutboxSource+"good", 10); len(other) != 0 {
		t.Fatalf("DeadOutbox for another source = %+v", other)
	}
	// Terminal: never due again, and still counted as unprocessed backlog.
	f.makeDue(t, "x-1")
	evidence.DrainUntilIdle(f.consumer, f.ctx)
	if f.tr.seen["x-1"] != domain.EvidenceOutboxMaxAttempts {
		t.Fatalf("a dead item was retried: %d deliveries", f.tr.seen["x-1"])
	}
}

func TestPgOutbox_TransientFailureRecovers(t *testing.T) {
	f := newPgOutbox(t)
	f.put(t, "flaky", "f-1")
	evidence.DrainUntilIdle(f.consumer, f.ctx)
	if r := f.row(t, "f-1"); r.attempts != 1 || r.processed {
		t.Fatalf("after one failure: %+v", r)
	}
	// Backing off: not in the pending read until its next attempt is due.
	pending, err := f.store.PendingOutbox(f.ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range pending {
		if ev, _ := f.store.Get(f.ctx, it.EvidenceID); ev != nil && ev.SourceKey == "f-1" && strings.HasPrefix(ev.SourceID, pgOutboxSource) {
			t.Fatal("a failed item is due again before its backoff passed")
		}
	}
	f.tr.healed = true
	f.makeDue(t, "f-1")
	evidence.DrainUntilIdle(f.consumer, f.ctx)
	if r := f.row(t, "f-1"); !r.processed || r.dead || r.attempts != 1 {
		t.Fatalf("recovered item: %+v", r)
	}
}

// A hold nobody released comes back on the recheck, without an attempt.
func TestPgOutbox_UnreleasedHoldIsRechecked(t *testing.T) {
	f := newPgOutbox(t)
	f.tr.paused = true
	f.put(t, "paused", "r-1")
	evidence.DrainUntilIdle(f.consumer, f.ctx)
	f.tr.paused = false
	f.makeDue(t, "r-1") // the recheck time has passed
	evidence.DrainUntilIdle(f.consumer, f.ctx)
	if r := f.row(t, "r-1"); !r.processed || r.attempts != 0 {
		t.Fatalf("rechecked item: %+v", r)
	}
}

// Replay (owner decision 2026-11-13,
// T-20261113-dead-evidence-outbox-items-have-no-replay-path): a dead item whose
// cause was fixed is reset to pending and the ORDINARY consumer processes it;
// replaying again, or replaying an item that is not dead, changes nothing; an
// evidence id is only honoured inside the named source.
func TestPgOutbox_ReplayDeadRunsTheNormalConsumerAgain(t *testing.T) {
	f := newPgOutbox(t)
	f.put(t, "flaky", "rd-1")
	f.put(t, "flaky", "rd-2")
	f.put(t, "good", "rd-ok")
	for range domain.EvidenceOutboxMaxAttempts {
		evidence.DrainUntilIdle(f.consumer, f.ctx)
		f.makeDue(t, "rd-1")
		f.makeDue(t, "rd-2")
	}
	for _, k := range []string{"rd-1", "rd-2"} {
		if r := f.row(t, k); !r.dead {
			t.Fatalf("%s did not die: %+v", k, r)
		}
	}
	evID := func(key string) domain.EvidenceID {
		var id string
		if err := f.pool.QueryRow(f.ctx, `SELECT id FROM evidence WHERE source_id LIKE 'outboxpg:%' AND source_key = $1`, key).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return domain.EvidenceID(id)
	}

	if _, err := f.store.ReplayDeadOutbox(f.ctx, "", ""); err == nil {
		t.Fatal("a replay with no source was accepted")
	}
	// Another source's name with this evidence id: nothing.
	if got, err := f.store.ReplayDeadOutbox(f.ctx, pgOutboxSource+"good", evID("rd-1")); err != nil || len(got) != 0 {
		t.Fatalf("cross-source replay = %+v, %v; want nothing", got, err)
	}
	// A processed item: a no-op, not an error.
	if got, err := f.store.ReplayDeadOutbox(f.ctx, pgOutboxSource+"good", evID("rd-ok")); err != nil || len(got) != 0 {
		t.Fatalf("replay of a processed item = %+v, %v; want nothing", got, err)
	}

	// One delivery.
	f.tr.healed = true
	got, err := f.store.ReplayDeadOutbox(f.ctx, pgOutboxSource+"flaky", evID("rd-1"))
	if err != nil || len(got) != 1 || got[0].EvidenceID != evID("rd-1") ||
		got[0].Attempts != domain.EvidenceOutboxMaxAttempts || got[0].LastError != "briefly down" || got[0].DeadAt.IsZero() {
		t.Fatalf("replay rd-1 = %+v, %v; want the one item as it was when dead", got, err)
	}
	if r := f.row(t, "rd-1"); r.dead || r.processed || r.attempts != 0 || r.lastErr != "" || r.held != "" || r.dueIn != 0 {
		t.Fatalf("rd-1 after replay = %+v, want pending, due now, attempts cleared", r)
	}
	if r := f.row(t, "rd-2"); !r.dead {
		t.Fatalf("a single-delivery replay touched rd-2: %+v", r)
	}
	// Idempotent: the same replay again finds nothing.
	if again, err := f.store.ReplayDeadOutbox(f.ctx, pgOutboxSource+"flaky", evID("rd-1")); err != nil || len(again) != 0 {
		t.Fatalf("second replay = %+v, %v; want a no-op", again, err)
	}
	evidence.DrainUntilIdle(f.consumer, f.ctx)
	if r := f.row(t, "rd-1"); !r.processed || r.dead {
		t.Fatalf("replayed rd-1 was not processed by the consumer: %+v", r)
	}

	// The whole source.
	got, err = f.store.ReplayDeadOutbox(f.ctx, pgOutboxSource+"flaky", "")
	if err != nil || len(got) != 1 || got[0].EvidenceID != evID("rd-2") {
		t.Fatalf("source replay = %+v, %v; want rd-2 only (rd-1 is processed)", got, err)
	}
	evidence.DrainUntilIdle(f.consumer, f.ctx)
	if r := f.row(t, "rd-2"); !r.processed {
		t.Fatalf("replayed rd-2 was not processed: %+v", r)
	}
	if dead, _ := f.store.DeadOutbox(f.ctx, pgOutboxSource+"flaky", 10); len(dead) != 0 {
		t.Fatalf("DeadOutbox after replay = %+v", dead)
	}
}
