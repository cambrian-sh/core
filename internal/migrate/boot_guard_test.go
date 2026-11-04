package migrate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Boot-time migration guard (T-20261104-kernel-binaries-auto-apply-migrations-to-the).
// The incident it answers: a smoke build booted on the owner's working database and
// applied 0020-0022 on its way up. The acceptance is that a boot pointed at a protected
// database with pending migrations exits BEFORE ANY WRITE — so the integration tests
// below assert on the catalog, not just on the returned error.

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"": ModeAuto, "auto": ModeAuto, " AUTO ": ModeAuto,
		"refuse_pending": ModeRefusePending, "off": ModeOff} {
		got, err := ParseMode(in)
		if err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseMode("yes"); err == nil {
		t.Error("ParseMode(yes) should fail")
	}
}

func TestPolicyProtects_CaseInsensitive(t *testing.T) {
	p := Policy{ProtectedDBNames: []string{" Cambrian_DB "}}
	if !p.protects("cambrian_db") {
		t.Error("cambrian_db should be protected")
	}
	if p.protects("cambrian_bench") {
		t.Error("cambrian_bench must not be protected by a cambrian_db entry")
	}
}

// guardPool connects to PG_TEST_DSN (a disposable database — same guard as the split
// test) and resets the public schema so every case starts from an empty database.
func guardPool(t *testing.T) (context.Context, *pgxpool.Pool, string) {
	t.Helper()
	ctx, pool := splitTestPool(t)
	// Extensions first: one DROP SCHEMA … CASCADE over vector/ltree/btree_gist objects
	// can fail with "cache lookup failed for function".
	rows, err := pool.Query(ctx, `SELECT extname FROM pg_extension WHERE extname <> 'plpgsql'`)
	if err != nil {
		t.Fatal(err)
	}
	var stmts []string
	for rows.Next() {
		var ext string
		if err := rows.Scan(&ext); err != nil {
			t.Fatal(err)
		}
		stmts = append(stmts, `DROP EXTENSION IF EXISTS "`+ext+`" CASCADE`)
	}
	rows.Close()
	stmts = append(stmts, `DROP SCHEMA IF EXISTS public CASCADE`, `CREATE SCHEMA public`)
	for _, stmt := range stmts {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("reset schema (%s): %v", stmt, err)
		}
	}
	db, err := currentDatabase(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, pool, db
}

func relationExists(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func appliedCount(ctx context.Context, t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func latestMigrations(t *testing.T, n int) []migration {
	t.Helper()
	migs, err := loadMigrations(splitTestDim)
	if err != nil {
		t.Fatal(err)
	}
	return migs[len(migs)-n:]
}

// A fresh protected database: refused, and not even schema_migrations is created.
func TestBoot_ProtectedFreshDB_RefusesBeforeAnyWrite(t *testing.T) {
	ctx, pool, db := guardPool(t)
	err := Boot(ctx, pool, splitTestDim, Policy{ProtectedDBNames: []string{db}})
	if !errors.Is(err, ErrPendingRefused) {
		t.Fatalf("want ErrPendingRefused, got %v", err)
	}
	for _, want := range []string{db, "0001_baseline", "protected_dbnames",
		"migrate up", "CAMBRIAN_DATABASE__MIGRATIONS__ALLOW_PENDING=true"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}
	if relationExists(ctx, t, pool, "schema_migrations") || relationExists(ctx, t, pool, "chunks") {
		t.Fatal("a refused boot wrote to the database")
	}
}

// The incident shape: an operator database a few migrations behind the binary. The
// refusal names exactly the pending versions and leaves schema_migrations untouched.
func TestBoot_ProtectedDBBehind_NamesPendingAndWritesNothing(t *testing.T) {
	ctx, pool, db := guardPool(t)
	tail := latestMigrations(t, 2)
	applyThrough(ctx, t, pool, tail[0].version-1)
	// A real operator database also carries the concurrent indexes of what it applied.
	applied, err := appliedVersions(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureConcurrentIndexes(ctx, pool, db, applied, concurrentIndexesFor(splitTestDim)); err != nil {
		t.Fatal(err)
	}
	before := appliedCount(ctx, t, pool)

	err = Boot(ctx, pool, splitTestDim, Policy{Mode: ModeAuto, ProtectedDBNames: []string{db}})
	if !errors.Is(err, ErrPendingRefused) {
		t.Fatalf("want ErrPendingRefused, got %v", err)
	}
	if !strings.Contains(err.Error(), "2 pending schema change(s)") {
		t.Errorf("want exactly 2 pending changes named: %v", err)
	}
	for _, m := range tail {
		name := m.name
		if !strings.Contains(err.Error(), name) {
			t.Errorf("refusal does not name pending migration %q: %v", name, err)
		}
	}
	if got := appliedCount(ctx, t, pool); got != before {
		t.Fatalf("schema_migrations changed under a refusal: %d -> %d", before, got)
	}

	// The explicit opt-in applies them; a later guarded boot with nothing pending boots.
	if err := Boot(ctx, pool, splitTestDim, Policy{ProtectedDBNames: []string{db}, AllowPending: true}); err != nil {
		t.Fatalf("opt-in boot: %v", err)
	}
	if got := appliedCount(ctx, t, pool); got != before+2 {
		t.Fatalf("opt-in applied %d migrations, want 2", got-before)
	}
	if err := Boot(ctx, pool, splitTestDim, Policy{ProtectedDBNames: []string{db}}); err != nil {
		t.Fatalf("guarded boot with nothing pending must succeed: %v", err)
	}
}

// refuse_pending guards every database; auto on an unprotected one keeps the
// historical apply-everything behaviour; off touches nothing.
func TestBoot_Modes(t *testing.T) {
	ctx, pool, _ := guardPool(t)
	if err := Boot(ctx, pool, splitTestDim, Policy{Mode: ModeOff}); err != nil {
		t.Fatalf("off: %v", err)
	}
	if relationExists(ctx, t, pool, "schema_migrations") {
		t.Fatal("mode off wrote schema_migrations")
	}
	err := Boot(ctx, pool, splitTestDim, Policy{Mode: ModeRefusePending})
	if !errors.Is(err, ErrPendingRefused) || !strings.Contains(err.Error(), "mode is refuse_pending") {
		t.Fatalf("refuse_pending: want a refusal naming the mode, got %v", err)
	}
	if relationExists(ctx, t, pool, "schema_migrations") {
		t.Fatal("refuse_pending wrote schema_migrations")
	}
	// Unprotected, auto: a different database name on the list does not guard this one.
	if err := Boot(ctx, pool, splitTestDim, Policy{ProtectedDBNames: []string{"some_other_db"}}); err != nil {
		t.Fatalf("auto: %v", err)
	}
	migs, _ := loadMigrations(splitTestDim)
	if got := appliedCount(ctx, t, pool); got != len(migs) {
		t.Fatalf("auto applied %d of %d migrations", got, len(migs))
	}
	if err := Boot(ctx, pool, splitTestDim, Policy{Mode: ModeRefusePending}); err != nil {
		t.Fatalf("refuse_pending with nothing pending must boot: %v", err)
	}
}

// A missing concurrent index is a pending schema change too: refuse_pending must not
// build it behind the operator's back.
func TestBoot_MissingConcurrentIndexIsPending(t *testing.T) {
	ctx, pool, db := guardPool(t)
	if err := Migrate(ctx, pool, splitTestDim); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ix := concurrentIndexes[0].name
	if _, err := pool.Exec(ctx, "DROP INDEX "+ix); err != nil {
		t.Fatal(err)
	}
	err := Boot(ctx, pool, splitTestDim, Policy{ProtectedDBNames: []string{db}})
	if !errors.Is(err, ErrPendingRefused) || !strings.Contains(err.Error(), "index "+ix) {
		t.Fatalf("want a refusal naming index %s, got %v", ix, err)
	}
	if relationExists(ctx, t, pool, ix) {
		t.Fatal("a refused boot built the index")
	}
}
