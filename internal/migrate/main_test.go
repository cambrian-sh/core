package migrate

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
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
// (requireDisposableDB), so it can never touch a database the tests would not, and
// only when the database carried a Cambrian schema to begin with.
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

// origDimAtStart is the embedding dimension the shared database was migrated at
// before any test in this package ran (0 = nothing to restore).
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

// recordOriginalDim returns the dimension of chunks.embedding, or 0 when the
// database has no Cambrian schema (nothing to restore) or is not a disposable target.
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
	var vecType *string
	if err := pool.QueryRow(ctx, `SELECT format_type(a.atttypid, a.atttypmod) FROM pg_attribute a
		WHERE a.attrelid = to_regclass('public.chunks') AND a.attname = 'embedding'`).Scan(&vecType); err != nil || vecType == nil {
		return 0
	}
	if m := vectorDim.FindStringSubmatch(*vecType); m != nil {
		dim, _ := strconv.Atoi(m[1]) // the regexp admits digits only
		return dim
	}
	return 0
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
