package postgres

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/pgvector/pgvector-go"
)

// T-20261118-under-adr-0107-projection-read-by-id-reads end to end, through the
// production boot path on a scratch database: under ADR-0107 projection read every
// non-dense chunk read (GetByID, GetBatch, ChunksByIDs, QueryByMetadata,
// LexicalSearch, GetStaleMemories) hydrates `embedding` from chunk_embeddings for
// the ACTIVE model, as the dense Search already did. Before the fix they read the
// legacy chunks.embedding column: a chunk written since the switch came back with
// no vector, one written before with the previous model's. (Model switches here
// are at one dimension: a dimension change refuses to boot since ENG-2038.)
func TestPg_ProjectionRead_NonDenseReadsHydrateTheActiveModelsVector(t *testing.T) {
	cfgAt := dimScratchDB(t)
	ctx := context.Background()
	unit := func(dims, i int) []float32 { v := make([]float32, dims); v[i] = 1; return v }
	projected := func(dims int, model string) *config.Config {
		cfg := cfgAt(dims)
		cfg.Embedder.Model = model
		cfg.Execution.Retrieval.EmbeddingProjectionWrite = true
		cfg.Execution.Retrieval.EmbeddingProjectionRead = true
		return cfg
	}
	chunk := func(id, text string, vec []float32) *domain.Document {
		return &domain.Document{ID: id, Text: text, DocumentType: domain.DocTypeMemory,
			Metadata: map[string]interface{}{"eng2039": "yes"}, ActivationStrength: 0.1,
			Embedding: domain.Embedding{Vector: vec}}
	}
	bypass := &domain.TagPredicate{Bypass: true}
	type read struct {
		name string
		fn   func(p *PgVectorAdapter) map[string][]float32
	}
	byID := func(docs []domain.Document) map[string][]float32 {
		out := map[string][]float32{}
		for _, d := range docs {
			out[d.ID] = d.Embedding.Vector
		}
		return out
	}
	reads := []read{
		{"GetByID", func(p *PgVectorAdapter) map[string][]float32 {
			out := map[string][]float32{}
			for _, id := range []string{"eng2039-old", "eng2039-new"} {
				d, err := p.GetByID(ctx, id)
				if err != nil || d == nil {
					t.Fatalf("GetByID(%s) = %v, %v", id, d, err)
				}
				out[id] = d.Embedding.Vector
			}
			return out
		}},
		{"GetBatch", func(p *PgVectorAdapter) map[string][]float32 {
			docs, err := p.GetBatch(ctx, []string{"eng2039-old", "eng2039-new"})
			if err != nil {
				t.Fatalf("GetBatch: %v", err)
			}
			return byID(docs)
		}},
		{"ChunksByIDs", func(p *PgVectorAdapter) map[string][]float32 {
			docs, err := p.ChunksByIDs(ctx, []string{"eng2039-old", "eng2039-new"})
			if err != nil {
				t.Fatalf("ChunksByIDs: %v", err)
			}
			return byID(docs)
		}},
		{"QueryByMetadata", func(p *PgVectorAdapter) map[string][]float32 {
			docs, err := p.QueryByMetadata(ctx, map[string]string{"eng2039": "yes"}, 0)
			if err != nil {
				t.Fatalf("QueryByMetadata: %v", err)
			}
			return byID(docs)
		}},
		{"LexicalSearch", func(p *PgVectorAdapter) map[string][]float32 {
			res, err := p.LexicalSearch(ctx, "user", domain.SearchOptions{TopK: 10, Scope: bypass})
			if err != nil {
				t.Fatalf("LexicalSearch: %v", err)
			}
			docs := make([]domain.Document, 0, len(res))
			for _, r := range res {
				docs = append(docs, r.Document)
			}
			return byID(docs)
		}},
		{"GetStaleMemories", func(p *PgVectorAdapter) map[string][]float32 {
			docs, err := p.GetStaleMemories(ctx, 10)
			if err != nil {
				t.Fatalf("GetStaleMemories: %v", err)
			}
			return byID(docs)
		}},
	}
	expect := func(p *PgVectorAdapter, stage string, want map[string][]float32) {
		t.Helper()
		for _, r := range reads {
			got := r.fn(p)
			for id, w := range want {
				g, ok := got[id]
				if !ok {
					t.Fatalf("%s %s: %s missing from the result", stage, r.name, id)
				}
				if !equalVec(g, w) {
					t.Errorf("%s %s: %s embedding = %d dims (nonzero at %d), want %d dims (nonzero at %d)",
						stage, r.name, id, len(g), nonzeroAt(g), len(w), nonzeroAt(w))
				}
			}
		}
	}

	// Legacy mode (no projection): reads return the column, unchanged by the fix.
	legacy, err := NewPgVectorAdapter(ctx, cfgAt(1024))
	if err != nil {
		t.Fatalf("legacy boot at 1024: %v", err)
	}
	if err := legacy.SaveBatch(ctx, []*domain.Document{
		chunk("eng2039-old", "the user had a cat", unit(1024, 3)),
		chunk("eng2039-new", "the user has a dog", unit(1024, 4)),
	}); err != nil {
		t.Fatalf("legacy SaveBatch: %v", err)
	}
	expect(legacy, "legacy", map[string][]float32{"eng2039-old": unit(1024, 3), "eng2039-new": unit(1024, 4)})
	legacy.Close()

	// A model switch to m2 at the same dimension (since ENG-2038 a dimension change
	// refuses to boot). The legacy column still holds m1's vectors with the right
	// length — exactly the silent foreign-space cosine the ticket names. Neither
	// chunk has an m2 vector yet, so neither may come back with the legacy (m1) one.
	p, err := NewPgVectorAdapter(ctx, projected(1024, "m2"))
	if err != nil {
		t.Fatalf("projection boot m2@1024: %v", err)
	}
	expect(p, "m2 before re-embed", map[string][]float32{"eng2039-old": nil, "eng2039-new": nil})
	// A write under m2 lands in the projection; every read hydrates it.
	if err := p.SaveBatch(ctx, []*domain.Document{
		chunk("eng2039-old", "the user had a cat", unit(1024, 7)),
		chunk("eng2039-new", "the user has a dog", unit(1024, 5)),
	}); err != nil {
		t.Fatalf("SaveBatch m2@1024: %v", err)
	}
	expect(p, "m2", map[string][]float32{"eng2039-old": unit(1024, 7), "eng2039-new": unit(1024, 5)})
	p.Close()

	// Another switch, m3: without an m3 row a read returns no vector, never the
	// legacy column's (now m2's).
	same, err := NewPgVectorAdapter(ctx, projected(1024, "m3"))
	if err != nil {
		t.Fatalf("projection boot m3@1024: %v", err)
	}
	defer same.Close()
	expect(same, "m3 before re-embed", map[string][]float32{"eng2039-old": nil, "eng2039-new": nil})
	if err := same.Save(ctx, chunk("eng2039-new", "the user has a dog", unit(1024, 9))); err != nil {
		t.Fatalf("Save m3@1024: %v", err)
	}
	// A projection-only write (the shape cmd/embed-backfill leaves: chunk_embeddings,
	// not the legacy column): the read must take it over the legacy column (m2's).
	if _, err := same.pool.Exec(ctx, `INSERT INTO chunk_embeddings (chunk_id, model_id, dims, embedding)
		VALUES ($1, 'm3', 1024, $2)`, "eng2039-old", pgvector.NewVector(unit(1024, 11))); err != nil {
		t.Fatalf("projection-only m3 row: %v", err)
	}
	expect(same, "m3", map[string][]float32{"eng2039-old": unit(1024, 11), "eng2039-new": unit(1024, 9)})
}

func equalVec(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func nonzeroAt(v []float32) int {
	for i, x := range v {
		if x != 0 {
			return i
		}
	}
	return -1
}
