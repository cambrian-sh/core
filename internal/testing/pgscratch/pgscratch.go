// Package pgscratch gives a Postgres integration test its own schema on the shared
// PG_TEST_DSN database, so the test can start from empty tables without touching the
// migrated ones every other test needs (T-20261114-postgres-adapter-tests-drop-migrated-tables).
//
// Tests used to reset their fixture with `DROP TABLE conversations`, `TRUNCATE
// ontology_declarations` and the like. On a database provisioned by `orchestrator
// migrate up` those are migrated tables: a DROP left every later test in the same run
// without its table, and a TRUNCATE wiped rows a test running beside it had just
// written. The pattern that fixed ENG-890 (chunks_for_entities_pg_test.go) is
// generalised here: a scratch schema, first on every pooled connection's search_path,
// so the code under test resolves its unqualified table names to scratch tables.
//
// Two shapes:
//   - Isolated (Options.Shadow false): search_path is the scratch schema alone. The
//     test sees only what it creates there (Options.Migrations or its own DDL); use it
//     for a store whose tables are self-contained.
//   - Shadowing (Options.Shadow true): search_path is "<scratch>, public". The tables
//     created in the scratch schema shadow the migrated ones of the same name; every
//     other table still resolves to the migrated public one. Use it when only some
//     tables must start empty (e.g. the global, append-only ontology_declarations).
//
// The schema is dropped before use (a crashed earlier run cannot leak into this one)
// and again on cleanup. Schema names are fixed per call site, not random, so that a
// leftover is overwritten rather than accumulated; give each call site its own name.
package pgscratch

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Options configures one scratch schema.
type Options struct {
	// Shadow keeps public on the search_path after the scratch schema (see the package
	// comment). False isolates the test in the scratch schema alone.
	Shadow bool
	// Migrations are files in internal/migrate/migrations (e.g. "0002_conversations.sql")
	// applied, in order, into the scratch schema. The REAL files are read on purpose, so
	// the scratch tables cannot drift from the DDL that ships (ADR-0064).
	Migrations []string
}

var validSchema = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)

// Pool returns a pool on PG_TEST_DSN whose every connection resolves unqualified names in
// the scratch schema first. It skips the test when PG_TEST_DSN is unset (the message keeps
// "PG_TEST_DSN not set": CI's Postgres gate fails the job on it). The pool is closed and
// the schema dropped on cleanup.
func Pool(t testing.TB, schema string, opt Options) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping integration test that requires PostgreSQL")
	}
	if !validSchema.MatchString(schema) {
		t.Fatalf("pgscratch: schema %q must match %s", schema, validSchema)
	}
	ctx := context.Background()
	ident := pgx.Identifier{schema}.Sanitize()

	// Reset with a plain connection first: the pool's AfterConnect sets a search_path
	// naming the schema, which must exist by then.
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pgscratch: connect: %v", err)
	}
	_, err = admin.Exec(ctx, "DROP SCHEMA IF EXISTS "+ident+" CASCADE; CREATE SCHEMA "+ident)
	_ = admin.Close(ctx) // best-effort: the reset already succeeded or failed
	if err != nil {
		t.Fatalf("pgscratch: reset schema %s: %v", schema, err)
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("pgscratch: parse PG_TEST_DSN: %v", err)
	}
	searchPath := ident
	if opt.Shadow {
		searchPath += ", public"
	}
	// search_path is per session, so it is set on every new connection, not once.
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET search_path TO "+searchPath)
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pgscratch: pool: %v", err)
	}
	// Registered before pool.Close so it runs after it (LIFO) on a fresh connection.
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			t.Logf("pgscratch: drop schema %s: %v", schema, err)
			return
		}
		defer func() { _ = c.Close(context.Background()) }() // best-effort close
		if _, err := c.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+ident+" CASCADE"); err != nil {
			t.Logf("pgscratch: drop schema %s: %v", schema, err)
		}
	})
	t.Cleanup(pool.Close)

	for _, name := range opt.Migrations {
		ddl, err := os.ReadFile(filepath.Join(migrationsDir(t), name))
		if err != nil {
			t.Fatalf("pgscratch: read migration %s: %v", name, err)
		}
		if _, err := pool.Exec(ctx, string(ddl)); err != nil {
			t.Fatalf("pgscratch: apply migration %s into %s: %v", name, schema, err)
		}
	}
	return pool, ctx
}

// migrationsDir locates internal/migrate/migrations from this source file, so callers in
// any package can name migrations without a relative path of their own.
func migrationsDir(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("pgscratch: cannot locate the migrations directory")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "migrate", "migrations")
}
