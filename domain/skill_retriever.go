package domain

import "context"

// SkillRetriever ranks system skills by relevance to a query within the agent's
// effective scope and returns the top-k skill names (ADR-0046 D4). It is the
// skill-domain analog of ToolRetriever; the difference is scope-gating (ADR-0034)
// rather than grant-filtering — an agent only sees system skills its effective
// scope permits.
type SkillRetriever interface {
	Rank(ctx context.Context, query string, scope *EffectiveScope, k int) ([]string, error)
}

// VectorSkillRetriever is the pgvector-backed SkillRetriever. It embeds the query
// with the nomic search_query prefix, runs cosine search over DocTypeSkill, and
// passes the agent's effective scope through SearchOptions.Scope — the
// ScopedVectorStore / pgvector predicate enforces it, the SAME read path memory
// uses (no new permission logic, ADR-0046 D9). A relevance Floor returns up to k
// names, or none when nothing clears it.
type VectorSkillRetriever struct {
	Store    VectorStore
	Embedder Embedder
	Floor    float64 // minimum cosine (RawScore) to include; <=0 disables the floor
}

// Rank implements SkillRetriever.
func (r VectorSkillRetriever) Rank(ctx context.Context, query string, scope *EffectiveScope, k int) ([]string, error) {
	vec, err := r.Embedder.Embed(ctx, "search_query: "+query)
	if err != nil {
		return nil, err
	}
	results, err := r.Store.Search(ctx, vec, SearchOptions{
		DocumentType: DocTypeSkill,
		TopK:         k,
		Scope:        scope, // scope-gating delegated to the store (ADR-0034 chokepoint)
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, k)
	for _, res := range results {
		if r.Floor > 0 && res.RawScore < r.Floor {
			continue // relevance floor: below this, no skill fits (empty allowed)
		}
		out = append(out, res.Document.ID)
		if len(out) >= k {
			break
		}
	}
	return out, nil
}

var _ SkillRetriever = VectorSkillRetriever{}
