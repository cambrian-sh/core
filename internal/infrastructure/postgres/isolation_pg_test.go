package postgres

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// BRAIN-01 end-to-end: the isolation predicate must actually FILTER in Postgres,
// not merely render into a plausible SQL string.
//
// The rendered-shape tests next door assert the predicate looks right. This one
// asserts the database agrees — which is the only claim that matters, and the one
// a string comparison cannot make. It runs against a real server or skips.
func newIsoPool(t *testing.T) (*pgxpool.Pool, context.Context) {
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

	// A minimal stand-in for the documents table: this test is about the WHERE
	// clause, not about the embedding column, and building the full table would
	// couple it to migrations it does not exercise.
	if _, err := pool.Exec(ctx, `
DROP TABLE IF EXISTS brain01_iso_docs;
CREATE TABLE brain01_iso_docs (id TEXT PRIMARY KEY, metadata JSONB NOT NULL)`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS brain01_iso_docs`)
	})

	for _, row := range []struct{ id, meta string }{
		{"mine", `{"session_id":"S1","tags":["sales"]}`},
		{"theirs", `{"session_id":"S2","tags":["sales"]}`},
		{"corpus", `{"tags":["sales"]}`},
		{"corpus-empty-sid", `{"session_id":"","tags":["sales"]}`},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO brain01_iso_docs (id, metadata) VALUES ($1, $2::jsonb)`,
			row.id, row.meta); err != nil {
			t.Fatalf("seed %s: %v", row.id, err)
		}
	}
	return pool, ctx
}

func selectWith(t *testing.T, pool *pgxpool.Pool, ctx context.Context, iso *domain.SessionIsolation) map[string]bool {
	t.Helper()
	ds := dialect.From("brain01_iso_docs").Select("id")
	for _, expr := range isolationExpressions(iso, "") {
		ds = ds.Where(expr)
	}
	sql, args, err := ds.ToSQL()
	if err != nil {
		t.Fatalf("build sql: %v", err)
	}
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[id] = true
	}
	return got
}

// The failure BRAIN-01 exists to remove: another conversation's material must not
// come back from the database.
func TestPgIsolation_ExcludesAnotherSession(t *testing.T) {
	pool, ctx := newIsoPool(t)
	got := selectWith(t, pool, ctx, domain.IsolateTo("S1"))

	if got["theirs"] {
		t.Fatal("another session's document was returned by Postgres")
	}
	if !got["mine"] {
		t.Fatal("the conversation's own document was filtered out")
	}
	// Shared knowledge survives — a predicate, not a store reset. Both the
	// missing-key and the empty-string forms, because an exporter that writes
	// "session_id": "" is not the same row shape as one that omits the key, and
	// only one of them is caught by IS NULL.
	if !got["corpus"] || !got["corpus-empty-sid"] {
		t.Fatalf("unowned corpus was fenced off: %v", got)
	}
}

// The narrow form answers "what did THIS conversation produce".
func TestPgIsolation_ExcludeUnowned(t *testing.T) {
	pool, ctx := newIsoPool(t)
	got := selectWith(t, pool, ctx, &domain.SessionIsolation{SessionID: "S1"})

	if len(got) != 1 || !got["mine"] {
		t.Fatalf("IncludeUnowned=false returned %v, want only the session's own", got)
	}
}

// Bypass adds no predicate, so everything comes back.
func TestPgIsolation_BypassReturnsEverything(t *testing.T) {
	pool, ctx := newIsoPool(t)
	got := selectWith(t, pool, ctx, domain.IsolationBypass())
	if len(got) != 4 {
		t.Fatalf("bypass returned %d of 4: %v", len(got), got)
	}
}

// The SQL and the in-memory form must AGREE — the in-memory one is authoritative,
// and a divergence means one of them is enforcing something the other is not.
// This is the check that catches a future edit to either side.
func TestPgIsolation_AgreesWithInMemoryAllows(t *testing.T) {
	pool, ctx := newIsoPool(t)
	iso := domain.IsolateTo("S1")
	fromSQL := selectWith(t, pool, ctx, iso)

	inMemory := map[string]bool{}
	for id, meta := range map[string]map[string]interface{}{
		"mine":             {domain.MetaSessionID: "S1"},
		"theirs":           {domain.MetaSessionID: "S2"},
		"corpus":           {},
		"corpus-empty-sid": {domain.MetaSessionID: ""},
	} {
		if iso.Allows(meta) {
			inMemory[id] = true
		}
	}
	if len(fromSQL) != len(inMemory) {
		t.Fatalf("SQL and in-memory disagree:\n  sql=%v\n  mem=%v", fromSQL, inMemory)
	}
	for id := range inMemory {
		if !fromSQL[id] {
			t.Fatalf("in-memory admitted %q but SQL did not:\n  sql=%v\n  mem=%v", id, fromSQL, inMemory)
		}
	}
}
