package postgres

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strconv"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/migrate"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The boot path end to end (T-20261104-kernel-binaries-auto-apply-migrations-to-the):
// database.migrations in config reaches NewPgVectorAdapter, and a boot against a
// database named in protected_dbnames with pending migrations fails before it writes —
// the acceptance the incident ticket asked for, on a scratch database standing in for
// the operator's.
var guardDisposableDB = regexp.MustCompile(`(?i)test`)

func TestNewPgVectorAdapter_ProtectedDBWithPendingMigrations_RefusesBeforeAnyWrite(t *testing.T) {
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping integration test that requires PostgreSQL")
	}
	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cc := pc.ConnConfig
	if !guardDisposableDB.MatchString(cc.Database) {
		t.Skipf("refusing to reset the schema of %q: its name does not say test", cc.Database)
	}
	ctx := context.Background()
	admin, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close) // after the restore below (cleanups run last-in, first-out)
	// Leave the database as this test found it. The reset below drops the whole
	// schema and the boot under test migrates it at 4 dims; left that way, every
	// adapter test that runs after this file (activation, time, search filter,
	// the memory store package) meets a 4-dim chunks table and the destructive-
	// recreate guard refuses it (T-20261112-core-postgres-package-pg-tests-fail-on-a-fresh).
	// So: remember the embedding dimension the database was migrated at, and in
	// cleanup reset again and re-migrate at that dimension.
	var origDim int
	var vecType *string
	if err := admin.QueryRow(ctx, `SELECT format_type(a.atttypid, a.atttypmod) FROM pg_attribute a
		WHERE a.attrelid = to_regclass('public.chunks') AND a.attname = 'embedding'`).Scan(&vecType); err == nil && vecType != nil {
		if m := regexp.MustCompile(`^vector\((\d+)\)$`).FindStringSubmatch(*vecType); m != nil {
			origDim, _ = strconv.Atoi(m[1]) // the regexp admits digits only
		}
	}
	// The same reset the test itself performs: extensions first (CASCADE), then
	// the schema. Dropping the schema while an extension's objects still hang off
	// it fails with "cache lookup failed for function".
	resetSchema := func(pool *pgxpool.Pool) []string {
		var stmts []string
		if rows, err := pool.Query(ctx, `SELECT extname FROM pg_extension WHERE extname <> 'plpgsql'`); err == nil {
			for rows.Next() {
				var ext string
				if rows.Scan(&ext) == nil {
					stmts = append(stmts, `DROP EXTENSION IF EXISTS "`+ext+`" CASCADE`)
				}
			}
			rows.Close()
		}
		return append(stmts, `DROP SCHEMA IF EXISTS public CASCADE`, `CREATE SCHEMA public`, `CREATE EXTENSION IF NOT EXISTS vector`)
	}
	t.Cleanup(func() {
		// A FRESH pool: the test's own connections cached type and function ids
		// of the schema the reset dropped, and reuse fails with "cache lookup failed".
		fresh, err := pgxpool.NewWithConfig(ctx, pc.Copy())
		if err != nil {
			t.Errorf("restore pool: %v", err)
			return
		}
		defer fresh.Close()
		for _, s := range resetSchema(fresh) {
			if _, err := fresh.Exec(ctx, s); err != nil {
				t.Errorf("restore (%s): %v", s, err)
				return
			}
		}
		if origDim > 0 {
			if err := migrate.Boot(ctx, fresh, origDim, migrate.Policy{Mode: migrate.ModeAuto}); err != nil {
				t.Errorf("restore: re-migrate at %d dims: %v", origDim, err)
			}
		}
	})

	// A database with no Cambrian schema: every migration is pending.
	rows, err := admin.Query(ctx, `SELECT extname FROM pg_extension WHERE extname <> 'plpgsql'`)
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
	// The vector extension stays: the adapter's AfterConnect registers the vector type,
	// so it cannot connect to a database that lacks it at all (pre-existing, unrelated).
	stmts = append(stmts, `DROP SCHEMA IF EXISTS public CASCADE`, `CREATE SCHEMA public`,
		`CREATE EXTENSION IF NOT EXISTS vector`)
	for _, s := range stmts {
		if _, err := admin.Exec(ctx, s); err != nil {
			t.Fatalf("reset (%s): %v", s, err)
		}
	}

	cfg := &config.Config{}
	cfg.Database.Host = cc.Host
	cfg.Database.Port = strconv.Itoa(int(cc.Port))
	cfg.Database.User = cc.User
	cfg.Database.Password = cc.Password
	cfg.Database.DBName = cc.Database
	cfg.Embedder.Dimensions = 4
	cfg.Storage.AutoMigrate = true
	cfg.Database.Migrations.Mode = "auto"
	cfg.Database.Migrations.ProtectedDBNames = []string{cc.Database}

	if _, err := NewPgVectorAdapter(ctx, cfg); !errors.Is(err, migrate.ErrPendingRefused) {
		t.Fatalf("want migrate.ErrPendingRefused from a protected boot, got %v", err)
	}
	var exists bool
	if err := admin.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("a refused boot created schema_migrations")
	}

	// The same database, unprotected, boots and migrates as before the guard.
	cfg.Database.Migrations.ProtectedDBNames = nil
	p, err := NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		t.Fatalf("unprotected boot: %v", err)
	}
	p.pool.Close()
	if err := admin.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&exists); err != nil || !exists {
		t.Fatalf("unprotected boot did not migrate (exists=%v err=%v)", exists, err)
	}
}
