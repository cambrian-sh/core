package migrate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/internal/config"
)

// TestMain leaves the shared PG_TEST_DSN database as the package found it.
//
// Several tests here drop the whole public schema (guardPool) or migrate only part
// of the way (applyThrough + freshSchema, which resets the corpus tables but not
// the substrate ones). Each test is self-consistent, but the package as a whole
// left the database half-migrated: every package that ran after it on the same
// database — and premium's substrateplugin Pg tests in a local run that shares the
// database — met missing columns such as knowledge_items.valid_to
// (T-20261113-core-go-test-leaves-the-shared-pg-test-db-in-a,
// T-20261112-internal-migrate-boot-guard-test-leaves-its).
//
// So: before the tests, remember the embedding dimension the database was migrated
// at; after them, reset the schema and re-migrate at that dimension — the same
// restore internal/infrastructure/postgres/migration_guard_test.go does for its one
// test. It runs only under the SAME disposable-database guard the tests use
// (requireDisposableDB), so it can never touch a database the tests would not.
//
// A database that carried no Cambrian schema at start (an EMPTY PG_TEST_DSN, e.g. a
// fresh container) is left migrated to HEAD at sharedTestDBDim, not as the tests
// left it: they end it half-migrated at splitTestDim (4), and every package that ran
// after this one met missing columns or a kernel refusing to boot on a 4-dim store
// (T-20261118-core-pg-test-packages-fail-on-an-empty-pg-test).
func TestMain(m *testing.M) {
	code := m.Run()
	if err := restoreSharedSchema(); err != nil {
		fmt.Fprintf(os.Stderr, "internal/migrate: restore shared test database: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// sharedTestDBDim is the dimension the shared PG_TEST_DSN database is left at when it
// started empty: the default config's embedder.dimensions (1024), which the kernel and
// app Pg tests boot with (config.DefaultConfig) and whose boot refuses a store at any
// other dimension. internal/infrastructure/postgres migrates an empty database to the
// same dimension before its tests (testdim_test.go).
var sharedTestDBDim = config.DefaultConfig().Embedder.Dimensions

// origDimAtStart is the embedding dimension the shared database is re-migrated at
// after the tests: the one it was migrated at before any test in this package ran,
// sharedTestDBDim when it carried no Cambrian schema, 0 = nothing to restore (no
// disposable, reachable database).
var origDimAtStart = -1

func init() {
	// Capture the dimension before any test reshapes the database. init runs before
	// TestMain, so this sees the database exactly as the package found it.
	origDimAtStart = recordOriginalDim()
}

// disposableTarget mirrors requireDisposableDB without a *testing.T: the restore
// may act only where the tests themselves were allowed to act.
func disposableTarget(dsn string) bool {
	if dsn == "" {
		return false
	}
	if os.Getenv("PG_TEST_ALLOW_DESTRUCTIVE") == "1" {
		return true
	}
	db := databaseNameOf(dsn)
	return db != "" && splitTestDBPattern.MatchString(db)
}

var vectorDim = regexp.MustCompile(`^vector\((\d+)\)$`)

// recordOriginalDim returns the dimension of chunks.embedding, sharedTestDBDim when
// the database has no usable chunks.embedding yet (empty: the restore migrates it to
// HEAD), or 0 when it is unreachable or not a disposable target (nothing to restore).
func recordOriginalDim() int {
	dsn := os.Getenv("PG_TEST_DSN")
	if !disposableTarget(dsn) {
		return 0
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return 0
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return 0 // unreachable: the tests report it; there is nothing to restore
	}
	var vecType *string
	err = pool.QueryRow(ctx, `SELECT format_type(a.atttypid, a.atttypmod) FROM pg_attribute a
		WHERE a.attrelid = to_regclass('public.chunks') AND a.attname = 'embedding'`).Scan(&vecType)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0
	}
	if vecType != nil {
		if m := vectorDim.FindStringSubmatch(*vecType); m != nil {
			dim, _ := strconv.Atoi(m[1]) // the regexp admits digits only
			return dim
		}
	}
	// No chunks table (an empty database), or one without a typed embedding column:
	// end the package with the database migrated to HEAD, not half-way.
	return sharedTestDBDim
}

// restoreSharedSchema resets the schema (extensions first, as guardPool does: a
// single DROP SCHEMA CASCADE over vector/ltree/btree_gist objects can fail with
// "cache lookup failed for function") and re-migrates at the recorded dimension.
func restoreSharedSchema() error {
	if origDimAtStart <= 0 {
		return nil
	}
	dsn := os.Getenv("PG_TEST_DSN")
	if !disposableTarget(dsn) {
		return nil
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	rows, err := pool.Query(ctx, `SELECT extname FROM pg_extension WHERE extname <> 'plpgsql'`)
	if err != nil {
		return err
	}
	var stmts []string
	for rows.Next() {
		var ext string
		if err := rows.Scan(&ext); err != nil {
			rows.Close()
			return err
		}
		stmts = append(stmts, `DROP EXTENSION IF EXISTS "`+ext+`" CASCADE`)
	}
	rows.Close()
	stmts = append(stmts, `DROP SCHEMA IF EXISTS public CASCADE`, `CREATE SCHEMA public`)
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			return fmt.Errorf("reset (%s): %w", s, err)
		}
	}
	// A fresh pool: the old one's connections cached type OIDs from the dropped
	// extensions (the vector type), which a migration on them would trip over.
	fresh, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer fresh.Close()
	if err := Boot(ctx, fresh, origDimAtStart, Policy{Mode: ModeAuto}); err != nil {
		return fmt.Errorf("re-migrate at %d dims: %w", origDimAtStart, err)
	}
	return nil
}
