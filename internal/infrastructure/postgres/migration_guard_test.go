package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/migrate"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The boot path end to end (T-20261104-kernel-binaries-auto-apply-migrations-to-the):
// database.migrations in config reaches NewPgVectorAdapter, and a boot against a
// database named in protected_dbnames with pending migrations fails before it writes —
// the acceptance the incident ticket asked for, on a scratch database standing in for
// the operator's.
//
// The scratch database is the test's OWN (CREATE DATABASE on PG_TEST_DSN's server,
// dropped WITH (FORCE) after). It used to reset the shared PG_TEST_DSN database itself —
// DROP every extension and the public schema CASCADE, then re-migrate in cleanup — which
// destroyed every migrated table and row under any test running beside it and every
// row any earlier test had left (T-20261114-postgres-adapter-tests-drop-migrated-tables).
func TestNewPgVectorAdapter_ProtectedDBWithPendingMigrations_RefusesBeforeAnyWrite(t *testing.T) {
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping integration test that requires PostgreSQL")
	}
	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	server, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect PG_TEST_DSN: %v", err)
	}
	defer func() { _ = server.Close(ctx) }() // best-effort close
	name := fmt.Sprintf("cambrian_test_guard_%d", time.Now().UnixNano())
	if _, err := server.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create scratch database: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			t.Logf("drop %s: %v", name, err)
			return
		}
		defer func() { _ = c.Close(context.Background()) }() // best-effort close
		if _, err := c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Logf("drop %s: %v", name, err)
		}
	})
	cc := pc.ConnConfig
	cc.Database = name
	admin, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close) // before the DROP DATABASE above (cleanups run last-in, first-out)

	// A database with no Cambrian schema: every migration is pending. The vector
	// extension is created: the adapter's AfterConnect registers the vector type,
	// so it cannot connect to a database that lacks it at all (pre-existing, unrelated).
	if _, err := admin.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		t.Fatalf("create vector extension: %v", err)
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
