package postgres

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cambrian-sh/core/domain"
)

// ChunksForEntities end-to-end: the batched corroboration + relevance query must
// actually RUN in Postgres — the goqu `IN ?` expansion inside a CASE and the
// grouped vector-distance ORDER are exactly the constructs a rendered-shape
// assertion cannot vouch for. Runs against PG_TEST_DSN (pointed at a scratch
// database: it creates the real-named chunks/chunk_triplets tables) or skips.
func TestChunksForEntities_LivePG(t *testing.T) {
	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping integration test that requires PostgreSQL")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, `
DROP TABLE IF EXISTS chunk_triplets;
DROP TABLE IF EXISTS chunks;
CREATE TABLE chunks (
  id TEXT PRIMARY KEY,
  metadata JSONB NOT NULL DEFAULT '{}',
  embedding vector(3)
);
CREATE TABLE chunk_triplets (
  chunk_id TEXT NOT NULL,
  h TEXT NOT NULL, r TEXT NOT NULL, t TEXT NOT NULL,
  weight DOUBLE PRECISION NOT NULL DEFAULT 1,
  extracted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (chunk_id, h, r, t)
);`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS chunk_triplets; DROP TABLE IF EXISTS chunks;`)
	})

	// c-both matches two query entities; c-acme and c-far match one each.
	// Embeddings: query [1,0,0]; c-acme near, c-far orthogonal.
	seed := `
INSERT INTO chunks (id, metadata, embedding) VALUES
 ('c-both', '{"tags":["open"]}', '[0.5,0.5,0]'),
 ('c-acme', '{"tags":["open"]}', '[1,0,0]'),
 ('c-far',  '{"tags":["open"]}', '[0,0,1]'),
 ('c-secret', '{"tags":["secret"]}', '[1,0,0]');
INSERT INTO chunk_triplets (chunk_id, h, r, t) VALUES
 ('c-both', 'acme', 'supplies', 'widgets'),
 ('c-both', 'globex', 'buys', 'widgets'),
 ('c-acme', 'acme', 'is', 'company'),
 ('c-far',  'globex', 'is', 'company'),
 ('c-secret', 'acme', 'hides', 'plans');`
	if _, err := pool.Exec(ctx, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	p := &PgVectorAdapter{pool: pool} // projectionRead=false → legacy embedding column
	scope := &domain.TagPredicate{ForbiddenTags: []string{"secret"}}
	hits, err := p.ChunksForEntities(ctx, []string{"acme", "globex"}, 10, scope, []float32{1, 0, 0})
	if err != nil {
		t.Fatalf("ChunksForEntities: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("want 3 hits (secret excluded), got %d: %+v", len(hits), hits)
	}
	if hits[0].ChunkID != "c-both" || hits[0].Matches != 2 {
		t.Errorf("corroborated chunk must rank first with Matches=2: %+v", hits[0])
	}
	if hits[1].ChunkID != "c-acme" {
		t.Errorf("query-near chunk must beat orthogonal at equal corroboration: %+v", hits)
	}
	for _, h := range hits {
		if h.ChunkID == "c-secret" {
			t.Errorf("forbidden-tagged chunk leaked: %+v", hits)
		}
	}

	// Ranked single-entity variant: relevance order, not extraction recency.
	ids, err := p.ChunksMentioningEntityRanked(ctx, "acme", 10, scope, []float32{1, 0, 0})
	if err != nil {
		t.Fatalf("ChunksMentioningEntityRanked: %v", err)
	}
	if len(ids) != 2 || ids[0] != "c-acme" {
		t.Errorf("ranked lookup must order by query cosine: %v", ids)
	}
}
