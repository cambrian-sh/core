package pgscratch

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

func schemaExists(t *testing.T, name string) bool {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, os.Getenv("PG_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(ctx) }() // best-effort close
	var ok bool
	if err := c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, name).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// Shadowing: a scratch table of the same name as a public one is what unqualified SQL
// reaches; the public table and its rows are untouched; the schema is gone afterwards.
func TestPool_ShadowLeavesPublicUntouchedAndDropsTheSchema(t *testing.T) {
	if os.Getenv("PG_TEST_DSN") == "" {
		t.Skip("PG_TEST_DSN not set; skipping integration test that requires PostgreSQL")
	}
	const schema = "pgscratch_self_test"
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, os.Getenv("PG_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Close(ctx) }() // best-effort close
	// A public table standing in for a migrated one, owned (and dropped) by this test.
	if _, err := admin.Exec(ctx, `DROP TABLE IF EXISTS public.pgscratch_probe;
CREATE TABLE public.pgscratch_probe (v TEXT); INSERT INTO public.pgscratch_probe VALUES ('public')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), `DROP TABLE IF EXISTS public.pgscratch_probe`) }) // test-owned table

	t.Run("shadow", func(t *testing.T) {
		pool, ctx := Pool(t, schema, Options{Shadow: true, Migrations: []string{"0040_ontology_declarations.sql"}})
		if _, err := pool.Exec(ctx, `CREATE TABLE pgscratch_probe (v TEXT); INSERT INTO pgscratch_probe VALUES ('scratch')`); err != nil {
			t.Fatal(err)
		}
		var v string
		if err := pool.QueryRow(ctx, `SELECT v FROM pgscratch_probe`).Scan(&v); err != nil || v != "scratch" {
			t.Fatalf("unqualified read = %q, %v; want the scratch table", v, err)
		}
		// The migration landed in the scratch schema, not public.
		var where string
		if err := pool.QueryRow(ctx, `SELECT relnamespace::regnamespace::text FROM pg_class WHERE oid = to_regclass('ontology_declarations')`).Scan(&where); err != nil || where != schema {
			t.Fatalf("ontology_declarations resolves to schema %q, %v; want %s", where, err, schema)
		}
		// Public tables still resolve through the shadowing path.
		if _, err := pool.Exec(ctx, `SELECT 1 FROM public.pgscratch_probe`); err != nil {
			t.Fatal(err)
		}
	})
	if schemaExists(t, schema) {
		t.Fatalf("schema %s survived its test", schema)
	}
	var v string
	if err := admin.QueryRow(ctx, `SELECT v FROM public.pgscratch_probe`).Scan(&v); err != nil || v != "public" {
		t.Fatalf("public table after the scratch test = %q, %v; want its own row untouched", v, err)
	}
}

// Isolated: nothing in public is visible, so an "unmigrated" guard can be exercised
// without dropping the real table.
func TestPool_IsolatedSeesNothingOfPublic(t *testing.T) {
	pool, ctx := Pool(t, "pgscratch_iso_test", Options{})
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_class c JOIN pg_namespace s ON s.oid = c.relnamespace
		WHERE s.nspname = current_schema()`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("fresh isolated schema holds %d relations, %v", n, err)
	}
	var reg *string
	if err := pool.QueryRow(ctx, `SELECT to_regclass('schema_migrations')::text`).Scan(&reg); err != nil || reg != nil {
		t.Fatalf("schema_migrations resolves to %v, %v; want nothing (public is off the path)", reg, err)
	}
}

func TestPool_RefusesAnUnsafeSchemaName(t *testing.T) {
	if validSchema.MatchString(`x"; DROP SCHEMA public; --`) || validSchema.MatchString("Public") || !validSchema.MatchString("conv_store_test") {
		t.Fatal("schema name validation")
	}
}
