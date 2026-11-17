package memory

import (
	"context"
	"reflect"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// cosineStore is scopeApplyingStore whose dense hits carry a fixed cosine, as the
// pgvector adapter's do (Score = RawScore = 1 - cosine distance).
type cosineStore struct {
	scopeApplyingStore
	cos float64
}

func (s *cosineStore) Search(ctx context.Context, v []float32, opts domain.SearchOptions) ([]domain.SearchResult, error) {
	out, err := s.scopeApplyingStore.Search(ctx, v, opts)
	for i := range out {
		out[i].Score, out[i].RawScore = s.cos, s.cos
	}
	return out, err
}

// ENG-1953: SearchMatches is the single recall pass WITHOUT spreading: the rows the
// query matched, each keeping its cosine in RawScore. Search (spreading on) adds the
// graph-reached row and rebuilds every row from its activation energy, so RawScore is
// lost there (T-20261117-spreadandrank-drops-rawscore-so-workspace-min) — which is why
// the forget query preview floors on SearchMatches' hits.
func TestSearchMatches_NoSpreadingKeepsCosine(t *testing.T) {
	t.Parallel()
	qs := NewQueryService(&fakeEmbedder{}, &cosineStore{
		scopeApplyingStore: scopeApplyingStore{docs: []domain.Document{{ID: "seed", Text: "seed fact"}}},
		cos:                0.72,
	}, nil)
	qs.EnableSpreading(&fakeSpreader{extra: []domain.GraphNodeExpansion{
		{Document: domain.Document{ID: "linked", Text: "graph-linked fact"}, ActivationEnergy: 0.9},
	}})

	got, err := qs.SearchMatches(context.Background(), "q", "agent")
	if err != nil {
		t.Fatalf("SearchMatches: %v", err)
	}
	if !reflect.DeepEqual(ids(got), []string{"seed"}) || got[0].RawScore != 0.72 {
		t.Fatalf("SearchMatches = %+v; want only the matched seed, RawScore 0.72", got)
	}

	spread, err := qs.Search(context.Background(), "q", "agent")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(spread) != 2 {
		t.Fatalf("Search = %v; want the seed plus the graph-linked row", ids(spread))
	}
}
