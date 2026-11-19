package postgres

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/testing/pgscratch"
)

// ENG-210 end to end through the composition the kernel uses
// (NewPgVectorAdapter over a migrated database, config → adapter → Search):
// the dense pool feeds the activation re-rank, so a high-activation chunk
// ranked 16th by cosine wins top_k=1 with the default pool (20 rows) and is
// out of reach when retrieval.vector_pool_multiplier shrinks the pool to 10.
func TestPgVectorPool_FeedsTheActivationReRank(t *testing.T) {
	dsn, dim := pgscratch.Migrated(t)
	ctx := context.Background()
	const prefix, tag, axis = "eng210-", "eng210-pool", 7
	open := func(mult int) *PgVectorAdapter {
		t.Helper()
		cfg := config.DefaultConfig()
		pgscratch.ApplyToConfig(t, cfg, dsn, dim)
		cfg.Storage.AutoMigrate = false
		cfg.Execution.Retrieval.VectorPoolMultiplier = mult
		p, err := NewPgVectorAdapter(ctx, cfg)
		if err != nil {
			t.Fatalf("NewPgVectorAdapter: %v", err)
		}
		t.Cleanup(p.Close)
		return p
	}
	p := open(0) // the shipped default
	clean := func() {
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM chunks WHERE id LIKE 'eng210-%'`)
		_, _ = p.pool.Exec(context.Background(), `DELETE FROM documents WHERE id LIKE 'eng210-%'`)
	}
	clean()
	t.Cleanup(clean)

	// 30 chunks whose cosine to the query falls 1.00, 0.99, … ; only the 16th
	// (cosine 0.85) has activation 1.0, every other has 0. With floor 0.2 its
	// score is 0.85 against the best of the rest, 1.00 × 0.2.
	query := make([]float32, dim)
	query[axis] = 1
	const lifted = 15
	for i := 0; i < 30; i++ {
		c := 1 - 0.01*float64(i)
		v := make([]float32, dim)
		v[axis] = float32(c)
		v[axis+1+i] = float32(math.Sqrt(1 - c*c))
		act := 0.0
		if i == lifted {
			act = 1
		}
		if err := p.Save(ctx, &domain.Document{
			ID: fmt.Sprintf("%s%02d", prefix, i), DocumentType: domain.DocTypeMnemonicFact, Text: fmt.Sprintf("pool chunk %d", i),
			Embedding: domain.Embedding{Vector: v}, Metadata: map[string]any{"tags": []string{tag}}, ActivationStrength: act,
		}); err != nil {
			t.Fatal(err)
		}
	}
	scope := &domain.TagPredicate{RequiredTags: []string{tag}}
	search := func(p *PgVectorAdapter) string {
		t.Helper()
		res, err := p.Search(domain.WithScope(ctx, scope), query, domain.SearchOptions{
			DocumentType: domain.DocTypeMnemonicFact, TopK: 1, Scope: scope})
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 1 {
			t.Fatalf("got %d results", len(res))
		}
		return res[0].Document.ID
	}
	want := fmt.Sprintf("%s%02d", prefix, lifted)
	if got := search(p); got != want {
		t.Fatalf("default pool (20 rows): top-1 %s, want the activation-lifted %s", got, want)
	}
	if got := search(open(10)); got != prefix+"00" {
		t.Fatalf("pool 10 rows: top-1 %s, want %s00 (the lifted chunk is outside the pool)", got, prefix)
	}
	if got := search(open(30)); got != want {
		t.Fatalf("pool 30 rows: top-1 %s, want %s", got, want)
	}
}
