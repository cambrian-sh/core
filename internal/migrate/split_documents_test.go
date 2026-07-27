package migrate

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Integration test for 0007_split_documents.sql (ADR-0093).
//
// The migration reshapes the corpus the retrieval benchmarks measure against, so the thing
// worth proving is not that it runs — it is that NOTHING IS LOST and nothing lands in the
// wrong table. Every assertion below is a count or a value that would change if a row went
// missing or a WHERE clause drifted.
//
// It is destructive (it creates and drops schema objects), so it refuses to run against a
// database that does not look disposable. That guard exists because pointing destructive
// integration tests at the live database has already destroyed real data in this workspace.

var splitTestDBPattern = regexp.MustCompile(`(?i)(^|[_-])test($|[_-])|_test\b|\btest_`)

func requireDisposableDB(t *testing.T, dsn string) {
	t.Helper()
	if os.Getenv("PG_TEST_ALLOW_DESTRUCTIVE") == "1" {
		t.Log("PG_TEST_ALLOW_DESTRUCTIVE=1: operating on whatever this DSN points at")
		return
	}
	db := databaseNameOf(dsn)
	if db == "" {
		t.Skip("PG_TEST_DSN has no database name; refusing to reshape an unknown target")
	}
	if !splitTestDBPattern.MatchString(db) {
		t.Skipf("refusing to run the document-split migration in database %q — it does not look\n"+
			"disposable. This test renames the documents table and creates six others. Use a database\n"+
			"whose name says test (e.g. cambrian_split_test), or set PG_TEST_ALLOW_DESTRUCTIVE=1.", db)
	}
}

func databaseNameOf(dsn string) string {
	if i := strings.Index(dsn, "://"); i >= 0 {
		rest := dsn[i+3:]
		j := strings.Index(rest, "/")
		if j < 0 {
			return ""
		}
		db := rest[j+1:]
		if k := strings.IndexAny(db, "?#"); k >= 0 {
			db = db[:k]
		}
		return db
	}
	for _, f := range strings.Fields(dsn) {
		if strings.HasPrefix(f, "dbname=") {
			return strings.TrimPrefix(f, "dbname=")
		}
	}
	return ""
}

const splitTestDim = 4

// freshSchema drops everything this migration touches so the test starts from nothing.
// Ordered so dependent objects go before what they depend on.
func freshSchema(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS chunks, document_sections, tools, skills, agent_profiles CASCADE`,
		`DROP TABLE IF EXISTS documents, documents_legacy CASCADE`,
		`DROP TABLE IF EXISTS chunk_triplets, chunk_pagerank, chunk_pagerank_meta, document_edges CASCADE`,
		`DROP TABLE IF EXISTS schema_migrations CASCADE`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("reset (%s): %v", stmt, err)
		}
	}
}

// applyThrough runs migrations up to and including version `through`, so the test can seed
// the OLD schema before the split runs against it.
func applyThrough(ctx context.Context, t *testing.T, pool *pgxpool.Pool, through int64) {
	t.Helper()
	migs, err := loadMigrations(splitTestDim)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if err := ensureMigrationsTable(ctx, pool); err != nil {
		t.Fatalf("ensureMigrationsTable: %v", err)
	}
	// Skip what is already recorded, so a second call advances the schema instead of
	// replaying it — the test seeds between two calls, which is the whole point.
	applied, err := appliedVersions(ctx, pool)
	if err != nil {
		t.Fatalf("appliedVersions: %v", err)
	}
	for _, m := range migs {
		if m.version > through || applied[m.version] {
			continue
		}
		if err := applyMigration(ctx, pool, m); err != nil {
			t.Fatalf("apply %d: %v", m.version, err)
		}
	}
}

// seedLegacyCorpus writes one row of every kind the old table held, with the parentage and
// tag duplication the migration has to untangle.
func seedLegacyCorpus(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	rows := []struct {
		id, text, docType, meta string
	}{
		// Two chunks of ONE document, each carrying its own copy of the same tags — the
		// duplication this migration collapses into a single authoritative row.
		{"doc-a-chunk-1", "first chunk", "mnemonic_fact",
			`{"document_id":"doc-a","title":"Doc A","source_type":"markdown","tags":["airline","public"]}`},
		{"doc-a-chunk-2", "second chunk", "mnemonic_fact",
			`{"document_id":"doc-a","title":"Doc A","source_type":"markdown","tags":["airline","public"]}`},
		// A chunk of a different document, so grouping is actually exercised.
		{"doc-b-chunk-1", "other doc", "mnemonic_fact",
			`{"document_id":"doc-b","title":"Doc B","tags":["secrets"]}`},
		// An agent-written memory with NO parent document: must survive with a NULL FK.
		{"mem-1", "I called a tool", "mnemonic_action", `{"agent":"scout"}`},
		{"scene-1", "a scene", "mnemonic_scene", `{}`},
		{"ep-1", "session narrative", "episodic_memory", `{}`},
		{"legacy-1", "old row", "memory", `{}`},
		// A structural node, which is not embedded and belongs in its own table.
		{"doc-a-sec-1", "Section One", "doc_section", `{"document_id":"doc-a","kind":"section"}`},
		// Seeded configuration, rebuilt on every boot.
		{"tool-1", "a tool descriptor", "tool", `{"name":"search"}`},
		{"skill-1", "a skill descriptor", "skill", `{"name":"plan"}`},
		{"prof-1", "an agent profile", "agent_profile", `{"agent":"scout"}`},
	}
	for _, r := range rows {
		_, err := pool.Exec(ctx,
			`INSERT INTO documents (id, text, document_type, metadata, embedding, access_count, activation_strength)
			 VALUES ($1,$2,$3,$4::jsonb,$5,0,0.5)`,
			r.id, r.text, r.docType, r.meta, fmt.Sprintf("[%f,%f,%f,%f]", 0.1, 0.2, 0.3, 0.4))
		if err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}
}

func count(ctx context.Context, t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", q, err)
	}
	return n
}

func splitTestPool(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping the document-split integration test")
	}
	requireDisposableDB(t, dsn)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return ctx, pool
}

// The load-bearing test: every seeded row lands in exactly one new table, and the totals add
// up. A row silently dropped by a WHERE clause is the failure this is built to catch.
func TestSplitDocuments_MovesEveryRowExactlyOnce(t *testing.T) {
	ctx, pool := splitTestPool(t)
	freshSchema(ctx, t, pool)
	applyThrough(ctx, t, pool, 6)
	seedLegacyCorpus(ctx, t, pool)

	before := count(ctx, t, pool, `SELECT count(*) FROM documents`)
	if before != 11 {
		t.Fatalf("seed did not land: %d rows", before)
	}

	applyThrough(ctx, t, pool, 7)

	// Nothing was destroyed: the legacy table still holds everything it did.
	if got := count(ctx, t, pool, `SELECT count(*) FROM documents_legacy`); got != before {
		t.Errorf("documents_legacy = %d, want %d — the migration must not destroy the old corpus", got, before)
	}

	chunks := count(ctx, t, pool, `SELECT count(*) FROM chunks`)
	sections := count(ctx, t, pool, `SELECT count(*) FROM document_sections`)
	tools := count(ctx, t, pool, `SELECT count(*) FROM tools`)
	skills := count(ctx, t, pool, `SELECT count(*) FROM skills`)
	profiles := count(ctx, t, pool, `SELECT count(*) FROM agent_profiles`)

	if chunks != 7 {
		t.Errorf("chunks = %d, want 7 (3 doc chunks + 4 parentless memories)", chunks)
	}
	if sections != 1 || tools != 1 || skills != 1 || profiles != 1 {
		t.Errorf("sections/tools/skills/profiles = %d/%d/%d/%d, want 1 each",
			sections, tools, skills, profiles)
	}
	if total := chunks + sections + tools + skills + profiles; total != before {
		t.Errorf("rows across the new tables = %d, want %d — a row was dropped or duplicated", total, before)
	}
}

// The point of the whole migration: the document exists as a row, and its tags are the single
// authoritative copy rather than N duplicates.
func TestSplitDocuments_ReconstructsTheDocumentEntity(t *testing.T) {
	ctx, pool := splitTestPool(t)
	freshSchema(ctx, t, pool)
	applyThrough(ctx, t, pool, 6)
	seedLegacyCorpus(ctx, t, pool)
	applyThrough(ctx, t, pool, 7)

	if got := count(ctx, t, pool, `SELECT count(*) FROM documents`); got != 2 {
		t.Fatalf("documents = %d, want 2 (doc-a and doc-b) — the entity is reconstructed from chunk parentage", got)
	}

	var title string
	var tags []string
	if err := pool.QueryRow(ctx,
		`SELECT title, tags FROM documents WHERE id = 'doc-a'`).Scan(&title, &tags); err != nil {
		t.Fatalf("read doc-a: %v", err)
	}
	if title != "Doc A" {
		t.Errorf("title = %q, want %q", title, "Doc A")
	}
	// Two chunks each carried ["airline","public"]; the document must hold ONE copy.
	if len(tags) != 2 {
		t.Fatalf("doc-a tags = %v, want exactly the 2 distinct tags", tags)
	}
	seen := map[string]bool{tags[0]: true, tags[1]: true}
	if !seen["airline"] || !seen["public"] {
		t.Errorf("doc-a tags = %v, want airline+public", tags)
	}
}

// A chunk carved from a document gets a real foreign key; a memory an agent wrote about its
// own work has no parent and must keep a NULL rather than be forced under a synthetic one.
func TestSplitDocuments_ParentsChunksAndLeavesMemoriesUnparented(t *testing.T) {
	ctx, pool := splitTestPool(t)
	freshSchema(ctx, t, pool)
	applyThrough(ctx, t, pool, 6)
	seedLegacyCorpus(ctx, t, pool)
	applyThrough(ctx, t, pool, 7)

	if got := count(ctx, t, pool,
		`SELECT count(*) FROM chunks WHERE document_id = 'doc-a'`); got != 2 {
		t.Errorf("doc-a chunks = %d, want 2", got)
	}
	if got := count(ctx, t, pool,
		`SELECT count(*) FROM chunks WHERE document_id IS NULL`); got != 4 {
		t.Errorf("unparented chunks = %d, want 4 (action, scene, episodic, legacy memory)", got)
	}
	// The section is linked to its document too.
	if got := count(ctx, t, pool,
		`SELECT count(*) FROM document_sections WHERE document_id = 'doc-a'`); got != 1 {
		t.Errorf("doc-a sections = %d, want 1", got)
	}
}

// THE TRAP. Both stored functions name `documents` in their body and plpgsql resolves that at
// call time, so renaming the table without redefining them would leave decay and activation
// silently operating on the new, nearly-empty document table — every call succeeding and
// nothing happening. This asserts they act on chunks.
func TestSplitDocuments_StoredFunctionsFollowTheRowsToChunks(t *testing.T) {
	ctx, pool := splitTestPool(t)
	freshSchema(ctx, t, pool)
	applyThrough(ctx, t, pool, 6)
	seedLegacyCorpus(ctx, t, pool)
	applyThrough(ctx, t, pool, 7)

	var before float64
	if err := pool.QueryRow(ctx,
		`SELECT activation_strength FROM chunks WHERE id = 'doc-a-chunk-1'`).Scan(&before); err != nil {
		t.Fatalf("read activation: %v", err)
	}
	if _, err := pool.Exec(ctx, `SELECT update_activation_strength('doc-a-chunk-1', 0.2)`); err != nil {
		t.Fatalf("update_activation_strength: %v", err)
	}
	var after float64
	if err := pool.QueryRow(ctx,
		`SELECT activation_strength FROM chunks WHERE id = 'doc-a-chunk-1'`).Scan(&after); err != nil {
		t.Fatalf("re-read activation: %v", err)
	}
	if after <= before {
		t.Fatalf("activation did not move (%v -> %v): the function is still pointed at the old table", before, after)
	}

	// Decay must run against chunks without error and without touching the document entity,
	// which has no activation column at all.
	if _, err := pool.Exec(ctx, `SELECT apply_ebbinghaus_decay(30)`); err != nil {
		t.Fatalf("apply_ebbinghaus_decay: %v", err)
	}
}

// Re-running the migration must be a no-op rather than a second backfill.
func TestSplitDocuments_IsIdempotent(t *testing.T) {
	ctx, pool := splitTestPool(t)
	freshSchema(ctx, t, pool)
	applyThrough(ctx, t, pool, 6)
	seedLegacyCorpus(ctx, t, pool)
	applyThrough(ctx, t, pool, 7)

	chunks := count(ctx, t, pool, `SELECT count(*) FROM chunks`)
	docs := count(ctx, t, pool, `SELECT count(*) FROM documents`)

	migs, err := loadMigrations(splitTestDim)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	// Run the SQL directly rather than through applyMigration: the runner correctly refuses
	// to record a version twice, so going through it would test the runner's bookkeeping.
	// What matters here is that the STATEMENTS are safe to execute against a database that
	// has already been split — the case that arises whenever a migration is re-run by hand
	// after a partial failure.
	for _, m := range migs {
		if m.version == 7 {
			if _, err := pool.Exec(ctx, m.sql); err != nil {
				t.Fatalf("re-running the 0007 SQL must be safe: %v", err)
			}
		}
	}

	if got := count(ctx, t, pool, `SELECT count(*) FROM chunks`); got != chunks {
		t.Errorf("chunks after re-run = %d, want %d", got, chunks)
	}
	if got := count(ctx, t, pool, `SELECT count(*) FROM documents`); got != docs {
		t.Errorf("documents after re-run = %d, want %d", got, docs)
	}
}
