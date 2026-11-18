package postgres

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ENG-277: a projection-read kernel ensures its model's partial HNSW index on
// chunk_embeddings at boot (ADR-0107 D2), the index matches the read path's SQL so
// the planner can use it, the ensure is idempotent, and an INVALID index left by
// an interrupted concurrent build is rebuilt. Runs against PG_TEST_DSN (a migrated
// scratch database at 1536 dims) or skips.
func TestPg_ProjectionIndexEnsuredAtBoot(t *testing.T) {
	// Its own scratch database at 1536 (dimScratchDB skips without PG_TEST_DSN): the
	// shared test database is migrated at 1024, and since ENG-2038 a projection boot
	// at another dimension refuses while the descriptor columns are at 1024.
	const dims = 1536
	cfg := dimScratchDB(t)(dims)
	ctx := context.Background()
	u, err := url.Parse(os.Getenv("PG_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + cfg.Database.DBName
	admin, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()

	const model = "eng277-Test.model"
	name := ProjectionIndexName(model)
	drop := func() {
		if _, err := admin.Exec(ctx, `DROP INDEX IF EXISTS "`+name+`"`); err != nil {
			t.Fatalf("drop: %v", err)
		}
	}
	drop()
	defer drop()

	valid := func() (bool, bool) {
		var v bool
		err := admin.QueryRow(ctx, `SELECT i.indisvalid FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid WHERE c.relname = $1`, name).Scan(&v)
		if err != nil {
			return false, false
		}
		return true, v
	}

	cfg.Embedder.Model = model
	cfg.Storage.AutoMigrate = true
	cfg.Database.Migrations.Mode = "auto"
	cfg.Execution.Retrieval.EmbeddingProjectionWrite = true
	cfg.Execution.Retrieval.EmbeddingProjectionRead = true

	p, err := NewPgVectorAdapter(ctx, cfg)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	p.Close()
	if exists, ok := valid(); !exists || !ok {
		t.Fatalf("boot must create a valid %s (exists=%v valid=%v)", name, exists, ok)
	}

	// The read path's shape uses it (seqscan disabled so a tiny table still shows
	// whether the index is applicable at all).
	conn, err := admin.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	vec := "[" + strings.TrimSuffix(strings.Repeat("0.1,", dims), ",") + "]"
	rows, err := conn.Query(ctx, `EXPLAIN SELECT ce.chunk_id FROM chunk_embeddings ce
		WHERE ce.model_id = '`+model+`' ORDER BY (ce.embedding::vector(1536)) <=> '`+vec+`'::vector LIMIT 5`)
	if err != nil {
		t.Fatal(err)
	}
	var plan strings.Builder
	for rows.Next() {
		var line string
		_ = rows.Scan(&line)
		plan.WriteString(line + "\n")
	}
	rows.Close()
	if !strings.Contains(plan.String(), name) {
		t.Fatalf("the read path's query does not use %s:\n%s", name, plan.String())
	}

	// Idempotent.
	if err := EnsureProjectionIndex(ctx, admin, model, dims); err != nil {
		t.Fatalf("second ensure: %v", err)
	}

	// An INVALID index is rebuilt, not kept by IF NOT EXISTS.
	if _, err := admin.Exec(ctx, `UPDATE pg_index SET indisvalid = false WHERE indexrelid = $1::regclass`, `"`+name+`"`); err != nil {
		t.Skipf("cannot mark the index invalid (needs superuser): %v", err)
	}
	if err := EnsureProjectionIndex(ctx, admin, model, dims); err != nil {
		t.Fatalf("ensure over an invalid index: %v", err)
	}
	if exists, ok := valid(); !exists || !ok {
		t.Fatalf("an invalid index must be rebuilt (exists=%v valid=%v)", exists, ok)
	}
}
