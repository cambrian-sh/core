package memory

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/infrastructure/postgres"
)

// constEmbedder embeds every text to the same unit vector, so the dense lane
// ties every chunk and the ranking is decided by the lexical lane.
type constEmbedder struct{ dims int }

func (e constEmbedder) Embed(context.Context, string) ([]float32, error) {
	v := make([]float32, e.dims)
	v[0] = 1
	return v, nil
}

// ENG-111 end to end through the retrieval path: a booted PgVectorAdapter on a
// freshly migrated database (migration 0064's stored text_tsv column), the
// enforcing reader, and QueryService.Search with hybrid dense+lexical fusion on,
// as the kernel composes them (retrieval_wiring.go EnableHybrid). The question's
// answer chunk lacks one of its words ("summer"), so no chunk carries every
// term: the default and_first lexical match finds nothing and falls back to the
// any-term match, which ranks the answer first, and RRF puts it on top.
func TestPg_HybridSearch_AndFirstFallsBackForAnAnswerMissingAQueryWord(t *testing.T) {
	cfgAt := factLaneScratchDB(t)
	ctx := context.Background()
	cfg := cfgAt(1024)
	p, err := postgres.NewPgVectorAdapter(ctx, cfg) // default retrieval config: lexical_match_mode and_first
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	defer p.Close()
	emb := constEmbedder{dims: 1024}
	vec, _ := emb.Embed(ctx, "")
	texts := []string{
		"Melanie painted a sunset for the charity auction.", // the answer: no "summer"
		"The team reviewed the budget spreadsheet on Monday.",
		"Caroline adopted a dog from the shelter.",
		"The office printer ran out of toner.",
		"A new coffee machine arrived in the kitchen.",
	}
	// The answer's vector is the furthest from the query (cosine 0.9 against
	// 1.0 for every distractor), so the dense lane alone ranks it last.
	far := make([]float32, 1024)
	far[0], far[1] = 0.9, float32(math.Sqrt(1-0.81))
	docs := make([]*domain.Document, len(texts))
	for i, text := range texts {
		v := vec
		if i == 0 {
			v = far
		}
		docs[i] = &domain.Document{ID: fmt.Sprintf("eng111-qa-%d", i), Text: text, DocumentType: domain.DocTypeMnemonicFact,
			ActivationStrength: 0.5, Embedding: domain.Embedding{Vector: v}}
	}
	if err := p.SaveBatch(ctx, docs); err != nil {
		t.Fatal(err)
	}
	const question = "What did Melanie paint for the charity auction last summer?"
	var all int
	if err := p.Pool().QueryRow(ctx, `SELECT count(*) FROM chunks WHERE text_tsv @@ plainto_tsquery('english', $1)`, question).Scan(&all); err != nil || all != 0 {
		t.Fatalf("precondition: no chunk may carry every term (n=%d, err=%v)", all, err)
	}

	sctx := domain.WithScope(ctx, &domain.TagPredicate{Bypass: true})
	denseOnly := NewQueryService(emb, authz.NewEnforcingVectorStore(p, nil), domain.AllowAllAuthorizer{})
	denseOnly.SetRecallSizes(3, 100)
	if res, err := denseOnly.Search(sctx, question, "alice"); err != nil || (len(res) > 0 && res[0].Document.ID == "eng111-qa-0") {
		t.Fatalf("precondition: the dense lane alone must not rank the answer first (err=%v)", err)
	}

	q := NewQueryService(emb, authz.NewEnforcingVectorStore(p, nil), domain.AllowAllAuthorizer{})
	q.EnableHybrid(p, 60)
	q.SetRecallSizes(3, 100)
	results, err := q.Search(sctx, question, "alice")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 || results[0].Document.ID != "eng111-qa-0" {
		ids := make([]string, len(results))
		for i, r := range results {
			ids[i] = r.Document.ID
		}
		t.Fatalf("top result %v, want eng111-qa-0 (found by the any-term fallback)", ids)
	}
}
