package migrate

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Tests for 0017_drop_removed_plugin_tables.sql (ADR-0064; owner decision 2026-10-31).
//
// The removed drift plugin created its tables at runtime, so the only honest test input is
// the schema it actually created: testdata/removed_drift_plugin_schema.sql is that DDL,
// verbatim from the deleted premium source. The test applies it to a database migrated to
// 0016 — the state of every database a premium kernel ran against — then proves 0017
// removes every table, index and sequence it made, and that running 0017 again (and the
// whole runner again) is a no-op.

const dropMigrationVersion int64 = 17

// The seven tables and every object they owned. Listed explicitly rather than matched by
// prefix so that a fixture which silently lost a CREATE also fails here.
var removedPluginTables = []string{
	"drift_records", "drift_record_pushes", "drift_commands", "drift_retention_runs",
	"drift_refusals", "drift_alerts", "drift_commitments",
}

func TestLoadMigrations_Includes0017DropRemovedPluginTables(t *testing.T) {
	migs, err := loadMigrations(4)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version == dropMigrationVersion {
			if m.name != "drop_removed_plugin_tables" {
				t.Fatalf("migration 17 is %q, want drop_removed_plugin_tables", m.name)
			}
			return
		}
	}
	t.Fatal("migration 0017_drop_removed_plugin_tables.sql is not embedded")
}

func TestDropRemovedPluginTables_RemovesEverythingAndIsIdempotent(t *testing.T) {
	ctx, pool := splitTestPool(t)
	freshSchema(ctx, t, pool)
	dropRemovedPluginTables(ctx, t, pool)
	t.Cleanup(func() { dropRemovedPluginTables(ctx, t, pool); freshSchema(ctx, t, pool) })

	// The state of a database a premium kernel ran against: core migrated to 0016, plus
	// the plugin's runtime tables.
	applyThrough(ctx, t, pool, dropMigrationVersion-1)
	ddl, err := os.ReadFile("testdata/removed_drift_plugin_schema.sql")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, string(ddl)); err != nil {
		t.Fatalf("create the removed plugin's tables: %v", err)
	}
	// Seed one row so the drop is proven against a non-empty table and the BIGSERIAL
	// sequence has actually been used.
	if _, err := pool.Exec(ctx,
		`INSERT INTO drift_records (kind, record_key, valid_from, observed_at, push_id, transport)
		 VALUES ('purchase_order', 'PO-1', now(), now(), 'p1', 'test')`); err != nil {
		t.Fatalf("seed drift_records: %v", err)
	}
	// 7 tables + 7 primary-key indexes + 10 named secondary indexes + 5 BIGSERIAL
	// sequences (records, retention_runs, refusals, alerts, commitments). Exact, so a
	// fixture that lost a CREATE cannot make the post-drop assertion pass vacuously.
	if n := countRemovedPluginObjects(ctx, t, pool); n != 7+7+10+5 {
		t.Fatalf("fixture created %d drift_* objects, want 29", n)
	}

	// Apply 0017 through the real runner, exactly as boot / `migrate up` does.
	if err := Migrate(ctx, pool, splitTestDim); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if n := countRemovedPluginObjects(ctx, t, pool); n != 0 {
		t.Fatalf("after 0017, %d drift_* objects remain; want 0", n)
	}
	if n := count(ctx, t, pool, `SELECT count(*) FROM schema_migrations WHERE version = $1`,
		dropMigrationVersion); n != 1 {
		t.Fatalf("schema_migrations has %d rows for version 17, want 1", n)
	}

	// Idempotency, both ways it can be exercised: the runner again (no pending work), and
	// the file's SQL replayed directly against a database that no longer has the tables.
	if err := Migrate(ctx, pool, splitTestDim); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	migs, err := loadMigrations(splitTestDim)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version == dropMigrationVersion {
			if _, err := pool.Exec(ctx, m.sql); err != nil {
				t.Fatalf("replaying 0017 on an already-clean database: %v", err)
			}
		}
	}

	// The core substrate tables the plugin also wrote into must survive (ADR-0106).
	for _, keep := range []string{"knowledge_items", "statement_values", "resolutions"} {
		if n := count(ctx, t, pool,
			`SELECT count(*) FROM pg_class WHERE relname = $1 AND relkind = 'r'`, keep); n != 1 {
			t.Fatalf("core table %s missing after 0017", keep)
		}
	}
}

// countRemovedPluginObjects counts every relation (table, index, sequence) the removed
// plugin could have left: everything in pg_class whose name starts with drift_.
func countRemovedPluginObjects(ctx context.Context, t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	return count(ctx, t, pool, `SELECT count(*) FROM pg_class WHERE relname LIKE 'drift\_%'`)
}

// dropRemovedPluginTables resets the fixture so a failed run cannot leak into the next.
func dropRemovedPluginTables(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, tbl := range removedPluginTables {
		if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS `+tbl); err != nil {
			t.Fatalf("reset %s: %v", tbl, err)
		}
	}
}

// TestDropRemovedPluginTables_FreshDatabaseIsNoOp covers the OSS / CI / fresh-install case:
// a database that never ran the plugin migrates through 0017 without error.
func TestDropRemovedPluginTables_FreshDatabaseIsNoOp(t *testing.T) {
	ctx, pool := splitTestPool(t)
	freshSchema(ctx, t, pool)
	dropRemovedPluginTables(ctx, t, pool)
	t.Cleanup(func() { dropRemovedPluginTables(ctx, t, pool); freshSchema(ctx, t, pool) })
	if err := Migrate(ctx, pool, splitTestDim); err != nil {
		t.Fatalf("Migrate on a database without the plugin tables: %v", err)
	}
	if n := countRemovedPluginObjects(ctx, t, pool); n != 0 {
		t.Fatalf("%d drift_* objects on a fresh database", n)
	}
}
