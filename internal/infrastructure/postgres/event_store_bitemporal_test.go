package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// A point read answers with what HAPPENED last, not what ARRIVED last.
//
// `PointLookup` orders by `occurred_at DESC, id DESC`, and that single clause is
// what makes it safe to load a source's history at all: records from two years
// ago are inserted today, long after the current value, and a store that
// answered "most recently written" would let every backfilled record overwrite
// the present.
//
// The property was relied upon before it was tested. The customer-history gap
// note says so explicitly — "this is a genuinely valuable property and it should
// be protected by a test before anything here is built on top of it" — and the
// backfill was built first. This is that test, arriving late.
//
// It is written against the real store and a real database because the claim is
// about what Postgres returns. A test that asserted the SQL string contained
// "occurred_at DESC" would pass just as happily against a query that read from
// the wrong table.

const bitemporalNS = "test-bitemporal"

func newBitemporalStore(t *testing.T) (*PgEventStore, context.Context) {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping integration test that requires PostgreSQL")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// Scoped to its own namespace and cleaned up, so this cannot disturb — or be
	// disturbed by — anything else in a shared test database.
	clean := func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM observations WHERE namespace_id = $1`, bitemporalNS)
	}
	clean()
	t.Cleanup(clean)

	// An empty registry: this is about ordering, and declaring predicates here
	// would only add a validation path the ordering does not depend on.
	reg, err := domain.NewKindRegistry(nil, nil)
	if err != nil {
		t.Fatalf("kind registry: %v", err)
	}
	return NewPgEventStore(pool, reg), ctx
}

// record writes one observation, failing the test if the store refuses it.
func record(t *testing.T, s *PgEventStore, ctx context.Context, entity, predicate, value string, occurredAt time.Time, ref string) {
	t.Helper()
	if _, err := s.RecordObservation(ctx, domain.Observation{
		NamespaceID: bitemporalNS,
		EntityID:    entity,
		Predicate:   predicate,
		Value:       domain.StatementValue{Type: "text", Text: value},
		OccurredAt:  occurredAt,
		SourceRef:   ref,
	}); err != nil {
		t.Fatalf("record %s=%s: %v", predicate, value, err)
	}
}

// The backfill case, exactly: the present is recorded first, then history
// arrives. The current value must not move.
func TestPointLookupAnswersWithWhatHappenedLastNotWhatArrivedLast(t *testing.T) {
	s, ctx := newBitemporalStore(t)
	const entity, predicate = "PO-9001", "status"

	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	// What we already knew: the order shipped this morning.
	record(t, s, ctx, entity, predicate, "shipped", now, "live/1")

	// Then two years of history is loaded, newest-first — which is how most REST
	// APIs page, so it is the ordinary case rather than an adversarial one.
	record(t, s, ctx, entity, predicate, "packed", now.AddDate(0, 0, -1), "hist/3")
	record(t, s, ctx, entity, predicate, "paid", now.AddDate(-1, 0, 0), "hist/2")
	record(t, s, ctx, entity, predicate, "created", now.AddDate(-2, 0, 0), "hist/1")

	got, err := s.PointLookup(ctx, bitemporalNS, entity, predicate)
	if err != nil {
		t.Fatalf("PointLookup: %v", err)
	}
	if got == nil {
		t.Fatal("the observation vanished")
	}
	if got.Value.Text != "shipped" {
		t.Fatalf("a backfilled record from %s overwrote the present: the current status reads %q, want %q",
			got.OccurredAt.Format(time.RFC3339), got.Value.Text, "shipped")
	}
}

// And the other direction, which is the half that makes the first meaningful: a
// record that genuinely happened later must win even though it arrived in the
// middle of a fill. Without this the test above would also pass against a store
// that simply returned the oldest row.
func TestPointLookupPrefersTheLatestOccurrenceWhicheverOrderItArrived(t *testing.T) {
	s, ctx := newBitemporalStore(t)
	const entity, predicate = "PO-9002", "status"

	base := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	record(t, s, ctx, entity, predicate, "created", base.AddDate(0, 0, -30), "a/1")
	// Arrives last, and also happened last.
	record(t, s, ctx, entity, predicate, "delivered", base, "a/3")
	// Arrives after it, but happened earlier.
	record(t, s, ctx, entity, predicate, "in-transit", base.AddDate(0, 0, -1), "a/2")

	got, err := s.PointLookup(ctx, bitemporalNS, entity, predicate)
	if err != nil {
		t.Fatalf("PointLookup: %v", err)
	}
	if got == nil || got.Value.Text != "delivered" {
		t.Fatalf("want the latest OCCURRENCE, got %+v", got)
	}
}

// Two records claiming the same instant is not hypothetical — a source with
// day-granular timestamps produces it constantly, and it is exactly what the
// `id DESC` half of the clause is for. The tie-break must be deterministic;
// without one the answer flips between reads and nothing downstream is
// reproducible.
func TestPointLookupBreaksAnOccurredAtTieDeterministically(t *testing.T) {
	s, ctx := newBitemporalStore(t)
	const entity, predicate = "PO-9003", "status"

	sameInstant := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	record(t, s, ctx, entity, predicate, "first-written", sameInstant, "tie/1")
	record(t, s, ctx, entity, predicate, "last-written", sameInstant, "tie/2")

	first, err := s.PointLookup(ctx, bitemporalNS, entity, predicate)
	if err != nil {
		t.Fatalf("PointLookup: %v", err)
	}
	if first == nil {
		t.Fatal("the observation vanished")
	}
	// `id DESC` means the later-inserted row wins a tie. What is pinned here is
	// less which one and more that it is STABLE: ten reads must agree.
	for i := range 10 {
		again, err := s.PointLookup(ctx, bitemporalNS, entity, predicate)
		if err != nil {
			t.Fatalf("PointLookup %d: %v", i, err)
		}
		if again.Value.Text != first.Value.Text {
			t.Fatalf("a tied point read is not stable: got %q then %q", first.Value.Text, again.Value.Text)
		}
	}
	if first.Value.Text != "last-written" {
		t.Errorf("the tie-break should favour the later-inserted row, got %q", first.Value.Text)
	}
}

// History is ordered by occurrence too, so a fill that arrives out of order
// still reads back as a coherent story rather than in the order we happened to
// learn it.
func TestHistoryReadsInOccurrenceOrderNotInsertionOrder(t *testing.T) {
	s, ctx := newBitemporalStore(t)
	const entity, predicate = "PO-9004", "status"

	base := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	// Written newest-first, the way a backfill walking a source backwards would.
	record(t, s, ctx, entity, predicate, "delivered", base, "h/4")
	record(t, s, ctx, entity, predicate, "shipped", base.AddDate(0, 0, -2), "h/3")
	record(t, s, ctx, entity, predicate, "paid", base.AddDate(0, 0, -5), "h/2")
	record(t, s, ctx, entity, predicate, "created", base.AddDate(0, 0, -9), "h/1")

	rows, err := s.History(ctx, bitemporalNS, entity, predicate,
		base.AddDate(0, 0, -30), base.AddDate(0, 0, 1))
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("want the whole story, got %d row(s)", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].OccurredAt.Before(rows[i-1].OccurredAt) {
			t.Fatalf("history is not in occurrence order: %s at %s follows %s at %s",
				rows[i].Value.Text, rows[i].OccurredAt.Format(time.RFC3339),
				rows[i-1].Value.Text, rows[i-1].OccurredAt.Format(time.RFC3339))
		}
	}
}
