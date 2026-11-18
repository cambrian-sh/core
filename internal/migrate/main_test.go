package migrate

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The destructive Pg tests in this package run in their OWN database, never in the
// shared PG_TEST_DSN one (T-20261118-internal-migrate-pg-tests-cannot-run-in).
//
// They reshape a whole database: guardPool drops the public schema and every
// extension, freshSchema drops the corpus tables, and applyThrough applies migrations
// at splitTestDim (4) directly, outside the ENG-2029 advisory lock. Pointed at the
// shared database, any package running beside them on the same PG_TEST_DSN
// (internal/infrastructure/postgres, internal/kernel, app — `go test ./...` runs
// packages in parallel) met an empty, half-migrated or 4-dim schema and failed with
// `duplicate key ... pg_type_typname_nsp_index` on 0001. An earlier TestMain restore
// re-migrated the shared database afterwards; that fixed only what ran AFTER this
// package, not what ran beside it, and it is gone.
//
// So splitTestPool connects to one scratch database per package run, created on
// PG_TEST_DSN's server on first use (CREATE DATABASE, the same as lock_test.go's
// scratchDB and the app/postgres scratch helpers) and dropped WITH (FORCE) after the
// tests. The tests run sequentially inside the package and each resets the schema it
// needs, so one database per run is enough; the shared database is only connected to
// for CREATE/DROP DATABASE and is never written.

var pkgScratch struct {
	once sync.Once
	base string // PG_TEST_DSN: the server the scratch database lives on
	name string // the scratch database; "" until created
	err  error
}

// TestMain drops the package's scratch database, if a test created one.
func TestMain(m *testing.M) {
	code := m.Run()
	if err := dropPackageScratchDB(); err != nil {
		fmt.Fprintf(os.Stderr, "internal/migrate: drop scratch database %s: %v\n", pkgScratch.name, err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// packageScratchConfig returns a pool config for the package's scratch database,
// creating the database on first call. It skips without PG_TEST_DSN (the message keeps
// "PG_TEST_DSN not set": CI's Postgres gate fails the job on it).
func packageScratchConfig(t *testing.T) *pgxpool.Config {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping the migration integration tests")
	}
	pkgScratch.once.Do(func() {
		pkgScratch.base = dsn
		pkgScratch.err = createPackageScratchDB(dsn)
	})
	if pkgScratch.err != nil {
		t.Fatalf("create the package scratch database on PG_TEST_DSN's server: %v", pkgScratch.err)
	}
	cfg, err := pgxpool.ParseConfig(pkgScratch.base)
	if err != nil {
		t.Fatalf("parse PG_TEST_DSN: %v", err)
	}
	cfg.ConnConfig.Database = pkgScratch.name
	return cfg
}

func createPackageScratchDB(dsn string) error {
	ctx := context.Background()
	base, err := pgx.ParseConfig(dsn)
	if err != nil {
		return err
	}
	admin, err := pgx.ConnectConfig(ctx, base)
	if err != nil {
		return err
	}
	defer admin.Close(ctx)
	// pid + clock: unique across concurrent test binaries on one server. A name, not
	// a persisted record id (AGENTS.md §2.6 does not apply).
	name := fmt.Sprintf("cambrian_test_migrate_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		return err
	}
	pkgScratch.name = name
	return nil
}

func dropPackageScratchDB() error {
	if pkgScratch.name == "" {
		return nil
	}
	ctx := context.Background()
	base, err := pgx.ParseConfig(pkgScratch.base)
	if err != nil {
		return err
	}
	admin, err := pgx.ConnectConfig(ctx, base)
	if err != nil {
		return err
	}
	defer admin.Close(ctx)
	_, err = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pkgScratch.name+" WITH (FORCE)")
	return err
}
