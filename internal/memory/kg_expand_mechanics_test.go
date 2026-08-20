package memory

import (
	"context"
	"slices"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// Hop decay + corroboration: the floor drops with hop distance and rises with
// distinct-entity corroboration, bounded; cosine still lifts above the floor.
func TestExpandedScore_HopDecayAndCorroboration(t *testing.T) {
	doc := domain.Document{} // no embedding ⇒ pure floor
	if got := expandedScore(nil, doc, 0, 1); got != 0.5 {
		t.Errorf("hop-0 single-entity floor: got %v want 0.5", got)
	}
	h0, h1 := expandedScore(nil, doc, 0, 1), expandedScore(nil, doc, 1, 1)
	if h1 >= h0 {
		t.Errorf("floor must decay with hop: hop0=%v hop1=%v", h0, h1)
	}
	c1, c3 := expandedScore(nil, doc, 0, 1), expandedScore(nil, doc, 0, 3)
	if c3 <= c1 {
		t.Errorf("corroboration must raise the floor: c1=%v c3=%v", c1, c3)
	}
	// The bonus is bounded: corroboration 6 and 60 score identically.
	if a, b := expandedScore(nil, doc, 0, 6), expandedScore(nil, doc, 0, 60); a != b {
		t.Errorf("corroboration bonus must be capped: %v vs %v", a, b)
	}
	// Cosine lifts above the floor exactly as before.
	vec := []float32{1, 0}
	strong := domain.Document{Embedding: domain.Embedding{Vector: []float32{1, 0}}}
	if got := expandedScore(vec, strong, 1, 1); got != 1.0 {
		t.Errorf("cosine lift: got %v want 1.0", got)
	}
}

func TestLexicalAliasVariants(t *testing.T) {
	cases := map[string][]string{
		"kraków":        {"krakow"},
		"apples":        {"apple"},
		"acme's":        {"acme"},
		"coca-cola":     {"coca cola"},
		"united states": {"united state", "united-states"},
	}
	for in, want := range cases {
		got := lexicalAliasVariants(in)
		for _, w := range want {
			if !slices.Contains(got, w) {
				t.Errorf("variants(%q) = %v, missing %q", in, got, w)
			}
		}
	}
	if got := lexicalAliasVariants("us"); len(got) != 0 {
		t.Errorf("short token must not be plural-stripped into noise: %v", got)
	}
}

type fakeAliasIdx struct{ neighbors map[string][]string }

func (f *fakeAliasIdx) NeighborNamesOf(name string, k int) []string { return f.neighbors[name] }

// Alias expansion keeps original ranking first, adds semantic neighbors for top
// entities and lexical variants after, dedups, and respects the cap.
func TestExpandEntityAliases(t *testing.T) {
	idx := &fakeAliasIdx{neighbors: map[string][]string{"us": {"united states"}}}
	got := expandEntityAliases([]string{"us", "kraków"}, idx, 10)
	if got[0] != "us" || got[1] != "kraków" {
		t.Fatalf("originals must lead in rank order: %v", got)
	}
	for _, want := range []string{"united states", "krakow"} {
		if !slices.Contains(got, want) {
			t.Errorf("missing alias %q in %v", want, got)
		}
	}
	// Cap trims aliases, never originals.
	capped := expandEntityAliases([]string{"a1", "b2", "c3"}, idx, 3)
	if len(capped) != 3 || capped[0] != "a1" || capped[1] != "b2" || capped[2] != "c3" {
		t.Errorf("cap must preserve originals first: %v", capped)
	}
}

// The batched capability is preferred when the store implements it: its hits
// (with corroboration counts) are admitted in order and scored with the
// corroboration bonus, and the per-entity path is never queried.
type fakeBatchedStore struct {
	*fakeChunkTripletsStore // the per-entity fake existing tests use
	hits                    []domain.EntityChunkHit
	gotEntities             []string
	perEntityCalls          int
}

func (f *fakeBatchedStore) ChunksMentioningEntity(ctx context.Context, entity string, limit int, scope *domain.TagPredicate) ([]string, error) {
	f.perEntityCalls++
	return f.fakeChunkTripletsStore.ChunksMentioningEntity(ctx, entity, limit, scope)
}

func (f *fakeBatchedStore) ChunksForEntities(_ context.Context, entities []string, _ int, _ *domain.TagPredicate, _ []float32) ([]domain.EntityChunkHit, error) {
	f.gotEntities = entities
	return f.hits, nil
}

func TestKgExpand_PrefersBatchedLookupAndScoresCorroboration(t *testing.T) {
	inner := newFakeChunkTripletsStore()
	_ = inner.SaveChunkTriplets(context.Background(), "seed", []domain.ChunkTriplet{
		{H: "acme", R: "is", T: "supplier"},
	})
	store := &fakeBatchedStore{
		fakeChunkTripletsStore: inner,
		hits: []domain.EntityChunkHit{
			{ChunkID: "c-corroborated", Matches: 3},
			{ChunkID: "c-single", Matches: 1},
		},
	}
	vs := fakeVectorSearch{docs: map[string]domain.Document{
		"c-corroborated": {ID: "c-corroborated"},
		"c-single":       {ID: "c-single"},
	}}
	seeds := []domain.SearchResult{{Document: domain.Document{ID: "seed"}, Score: 0.9}}

	got := kgExpand(context.Background(), seeds, store, vs, nil, &domain.TagPredicate{Bypass: true},
		kgExpandOpts{Hops: 1, MaxExpanded: 10, MaxEntities: 10})

	if store.perEntityCalls != 0 {
		t.Errorf("batched store must not fall back to per-entity lookups; got %d calls", store.perEntityCalls)
	}
	if len(store.gotEntities) == 0 || store.gotEntities[0] != "acme" {
		t.Errorf("frontier entities must reach the batched lookup rank-first: %v", store.gotEntities)
	}
	if len(got) != 3 {
		t.Fatalf("want seed + 2 expanded, got %d results", len(got))
	}
	var corr, single float64
	for _, r := range got {
		switch r.Document.ID {
		case "c-corroborated":
			corr = r.Score
		case "c-single":
			single = r.Score
		}
	}
	if corr <= single {
		t.Errorf("corroborated chunk must outscore single-entity chunk: %v vs %v", corr, single)
	}
}
