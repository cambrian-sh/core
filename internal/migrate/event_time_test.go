package migrate

import (
	"testing"
)

// Tests for 0019_event_time.sql and the out-of-band event-time indexes (ADR-0134 P0).

const eventTimeMigrationVersion = 19

func TestLoadMigrations_Includes0019EventTime(t *testing.T) {
	migs, err := loadMigrations(4)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version == eventTimeMigrationVersion {
			if m.name != "event_time" {
				t.Fatalf("migration 19 is %q, want event_time", m.name)
			}
			return
		}
	}
	t.Fatal("migration 0019_event_time.sql is not embedded")
}

// The safety claim of 0019: on a POPULATED store it adds the columns without
// rewriting the table (no full-table rewrite of chunks under lock, no HNSW churn) and
// without backfilling. A rewrite allocates a new relfilenode, so an unchanged
// relfilenode is the proof. The old rows read as their ingest clock (NULL event_time).
func TestEventTimeMigration_NoRewriteNoBackfillOnAPopulatedTable(t *testing.T) {
	ctx, pool := splitTestPool(t)
	freshSchema(ctx, t, pool)
	t.Cleanup(func() { freshSchema(ctx, t, pool) })
	applyThrough(ctx, t, pool, 18)

	if _, err := pool.Exec(ctx, `
		INSERT INTO documents (id, title) SELECT 'd' || g, 't' FROM generate_series(1, 50) g;
		INSERT INTO chunks (id, document_id, text, embedding, metadata, created_at)
		SELECT 'c' || g, 'd' || (1 + g % 50), 'chunk ' || g, '[1,0,0,0]', '{}'::jsonb,
		       TIMESTAMP '2026-10-01 12:00:00' + g * INTERVAL '1 minute'
		FROM generate_series(1, 2000) g;`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	relfile := func(table string) uint32 {
		var n uint32
		if err := pool.QueryRow(ctx, `SELECT relfilenode FROM pg_class WHERE relname = $1 AND relnamespace = current_schema()::regnamespace`, table).Scan(&n); err != nil {
			t.Fatalf("relfilenode %s: %v", table, err)
		}
		return n
	}
	beforeChunks, beforeDocs := relfile("chunks"), relfile("documents")

	applyThrough(ctx, t, pool, eventTimeMigrationVersion)

	if a, b := relfile("chunks"), relfile("documents"); a != beforeChunks || b != beforeDocs {
		t.Fatalf("0019 rewrote a table: chunks %d->%d, documents %d->%d", beforeChunks, a, beforeDocs, b)
	}
	if n := count(ctx, t, pool, `SELECT count(*) FROM chunks WHERE event_time IS NOT NULL OR time_source IS NOT NULL`); n != 0 {
		t.Fatalf("0019 backfilled %d rows; it must leave old rows NULL", n)
	}
	// An old row's span is its created_at, read as a UTC instant.
	var same bool
	if err := pool.QueryRow(ctx, `SELECT `+EventStartExpr+` = TIMESTAMPTZ '2026-10-01 12:01:00+00' FROM chunks WHERE id = 'c1'`).Scan(&same); err != nil || !same {
		t.Fatalf("legacy row span start is not its UTC created_at (same=%v err=%v)", same, err)
	}
	for _, tbl := range []string{"chunks", "documents", "tools", "skills", "agent_profiles"} {
		if n := count(ctx, t, pool, `SELECT count(*) FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = $1
			  AND column_name IN ('event_time','event_time_end','time_source','time_precision','claimed_time')`, tbl); n != 5 {
			t.Fatalf("%s has %d of the 5 event-time columns", tbl, n)
		}
	}
	// The ingest clock defaults to a UTC wall clock whatever the session time zone.
	if _, err := pool.Exec(ctx, `SET TIME ZONE 'America/New_York'; INSERT INTO chunks (id, text) VALUES ('tz', 'x'); RESET TIME ZONE`); err != nil {
		t.Fatalf("insert under a non-UTC session: %v", err)
	}
	var skew float64
	if err := pool.QueryRow(ctx, `SELECT abs(extract(epoch FROM (created_at - (now() AT TIME ZONE 'UTC')))) FROM chunks WHERE id = 'tz'`).Scan(&skew); err != nil {
		t.Fatal(err)
	}
	if skew > 60 {
		t.Fatalf("created_at default is not UTC: %v s from the UTC wall clock", skew)
	}
}

// The window indexes are built by the runner OUTSIDE the migration transaction, are
// valid, are idempotent on a second run, and an interrupted (INVALID) build is
// detected and rebuilt rather than skipped by IF NOT EXISTS forever.
func TestEventTimeIndexes_BuiltConcurrentlyAndRebuiltWhenInvalid(t *testing.T) {
	ctx, pool := splitTestPool(t)
	freshSchema(ctx, t, pool)
	t.Cleanup(func() { freshSchema(ctx, t, pool) })

	if err := Migrate(ctx, pool, splitTestDim); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	validCount := func() int {
		return count(ctx, t, pool, `
			SELECT count(*) FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
			WHERE c.relname IN ('idx_chunks_event_start', 'idx_chunks_event_end') AND i.indisvalid`)
	}
	if n := validCount(); n != 2 {
		t.Fatalf("after Migrate: %d valid event-time indexes, want 2", n)
	}
	if err := Migrate(ctx, pool, splitTestDim); err != nil {
		t.Fatalf("second Migrate (idempotence): %v", err)
	}

	// Simulate a build interrupted mid-way: Postgres leaves the index INVALID.
	if _, err := pool.Exec(ctx, `UPDATE pg_index SET indisvalid = false
		WHERE indexrelid = 'idx_chunks_event_start'::regclass`); err != nil {
		t.Skipf("cannot mark the index invalid (needs superuser): %v", err)
	}
	if n := validCount(); n != 1 {
		t.Fatalf("setup: %d valid", n)
	}
	if err := Migrate(ctx, pool, splitTestDim); err != nil {
		t.Fatalf("Migrate with an invalid index: %v", err)
	}
	if n := validCount(); n != 2 {
		t.Fatalf("the INVALID index was not rebuilt: %d valid", n)
	}
}
