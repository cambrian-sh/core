package pgscratch

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/internal/migrate"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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

// TestParseVectorDim pins the type-name parsing the dimension probe depends on.
func TestParseVectorDim(t *testing.T) {
	for in, want := range map[string]int{
		"vector(1024)": 1024,
		"vector(1536)": 1536,
		"vector(4)":    4,
		"vector":       0,
		"halfvec(768)": 0,
		"":             0,
	} {
		if got := ParseVectorDim(in); got != want {
			t.Errorf("ParseVectorDim(%q) = %d, want %d", in, got, want)
		}
	}
}

// A database whose name does not say test is refused before any connection: the guard
// is a failure, not the skip that used to hide ~20 Pg tests (ENG-2100).
func TestEnsureMigrated_RefusesANonDisposableDatabase(t *testing.T) {
	for _, dsn := range []string{
		"postgres://u:p@127.0.0.1:1/cambrian?sslmode=disable",
		"host=127.0.0.1 port=1 user=u dbname=prod sslmode=disable",
	} {
		if _, err := EnsureMigrated(context.Background(), dsn); err == nil || !strings.Contains(err.Error(), "refusing to touch") {
			t.Errorf("EnsureMigrated(%q) = %v, want a refusal", dsn, err)
		}
	}
	if err := CheckDisposable("postgres://u:p@127.0.0.1:1/cambrian_test?sslmode=disable"); err != nil {
		t.Errorf("cambrian_test refused: %v", err)
	}
}

// scratchDatabase creates an empty database on the PG_TEST_DSN server and returns its
// DSN; it is dropped on cleanup.
func scratchDatabase(t *testing.T, name string) string {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping integration test that requires PostgreSQL")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		t.Fatal(err)
	}
	_ = admin.Close(ctx) // best-effort: reconnected for the drop below
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), dsn)
		if err != nil {
			t.Logf("drop %s: %v", name, err)
			return
		}
		defer func() { _ = c.Close(context.Background()) }() // best-effort close
		if _, err := c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)"); err != nil {
			t.Logf("drop %s: %v", name, err)
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

// An empty database is migrated to HEAD at the shared dimension (1024, the default
// config's), the evidence outbox's tables exist afterwards (the ENG-2100 failure was
// `relation "evidence" does not exist`), and a second call is a no-op.
func TestEnsureMigrated_EmptyDatabaseAtTheSharedDimension(t *testing.T) {
	dsn := scratchDatabase(t, "pgscratch_ensure_empty_test")
	ctx := t.Context()
	dim, err := EnsureMigrated(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if dim != SharedTestDBDim || dim != 1024 {
		t.Fatalf("dim = %d, want the shared %d (1024)", dim, SharedTestDBDim)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, table := range []string{"evidence", "evidence_outbox", "principals", "memory_notes", "tool_grants"} {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass('public.'||$1) IS NOT NULL`, table).Scan(&ok); err != nil || !ok {
			t.Errorf("table %s after EnsureMigrated: exists=%v err=%v", table, ok, err)
		}
	}
	if got, err := probeChunksEmbeddingDim(ctx, pool); err != nil || got != 1024 {
		t.Errorf("chunks.embedding = vector(%d), %v; want 1024", got, err)
	}
	var pending int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&pending); err != nil || pending == 0 {
		t.Fatalf("schema_migrations rows = %d, %v", pending, err)
	}
	if again, err := EnsureMigrated(ctx, dsn); err != nil || again != dim {
		t.Fatalf("second EnsureMigrated = %d, %v", again, err)
	}
}

// A database already migrated at another dimension keeps it: the tests follow the
// database, never reshape it (T-20261113-core-postgres-pg-tests-fail-on-a-database).
func TestEnsureMigrated_FollowsTheDatabasesDimension(t *testing.T) {
	dsn := scratchDatabase(t, "pgscratch_ensure_1536_test")
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := migrate.Migrate(ctx, pool, 1536); err != nil {
		t.Fatal(err)
	}
	dim, err := EnsureMigrated(ctx, dsn)
	if err != nil || dim != 1536 {
		t.Fatalf("EnsureMigrated = %d, %v; want 1536", dim, err)
	}
	if got, err := probeChunksEmbeddingDim(ctx, pool); err != nil || got != 1536 {
		t.Errorf("chunks.embedding = vector(%d), %v; want 1536 untouched", got, err)
	}
}
