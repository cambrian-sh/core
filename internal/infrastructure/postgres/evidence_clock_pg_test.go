package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// evidence.ingested_at is the DATABASE's clock, not the host's
// (T-20261113-a-reconciling-fill-is-reported-caught-up-before, owner option (a)):
// ADR-0147 D14 compares it with a backfill's started_at, which Postgres stamps
// with now(), so both sides must read one clock.
//
// The skew is real, not simulated in Go: the test pool's search_path puts a
// schema whose now() is the server's clock plus two hours AHEAD of pg_catalog,
// so every now() the store's SQL calls on that connection runs two hours ahead
// of the host. A store that stamped the host clock lands ~2h behind what the
// database says; one that stamps the database clock lands on it.
func TestPg_EvidenceIngestedAtIsTheDatabaseClock(t *testing.T) {
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping integration test that requires PostgreSQL")
	}
	ctx := context.Background()
	const schema = "evidence_skewclock_test"
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	for _, q := range []string{
		`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`,
		`CREATE SCHEMA ` + schema,
		`CREATE FUNCTION ` + schema + `.now() RETURNS timestamptz LANGUAGE sql STABLE AS $$ SELECT pg_catalog.now() + interval '2 hours' $$`,
	} {
		if _, err := admin.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = admin.Exec(bg, `DELETE FROM evidence_outbox WHERE evidence_id IN (SELECT id FROM evidence WHERE source_id = 'skewclock')`)
		_, _ = admin.Exec(bg, `DELETE FROM evidence WHERE source_id = 'skewclock'`)
		_, _ = admin.Exec(bg, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	})

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ", pg_catalog, public"
	skewed, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(skewed.Close)
	var dbNow time.Time
	if err := skewed.QueryRow(ctx, `SELECT now()`).Scan(&dbNow); err != nil {
		t.Fatal(err)
	}
	host := time.Now()
	if dbNow.Sub(host) < 90*time.Minute {
		t.Fatalf("precondition: the skewed connection's now() must run ~2h ahead of the host (db %v, host %v)", dbNow, host)
	}

	store := NewPgEvidenceStore(skewed)
	id, inserted, err := store.Insert(ctx, domain.Evidence{SourceID: "skewclock", SourceKey: "k1",
		SourceRevision: "r1", ContentHash: "sha256:skew1"})
	if err != nil || !inserted {
		t.Fatalf("insert: %v %v", inserted, err)
	}
	ev, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if ev.IngestedAt.Sub(time.Now()) < 90*time.Minute {
		t.Fatalf("ingested_at %v is the host clock (host now %v); want the database clock (~%v)", ev.IngestedAt, time.Now(), dbNow)
	}

	// A caller-supplied receipt time is kept as given.
	given := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	id2, _, err := store.Insert(ctx, domain.Evidence{SourceID: "skewclock", SourceKey: "k2",
		SourceRevision: "r1", ContentHash: "sha256:skew2", IngestedAt: given})
	if err != nil {
		t.Fatal(err)
	}
	ev2, err := store.Get(ctx, id2)
	if err != nil || !ev2.IngestedAt.Equal(given) {
		t.Fatalf("explicit ingested_at: %v %v", ev2.IngestedAt, err)
	}
}
