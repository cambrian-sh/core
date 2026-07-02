package memory

import (
	"context"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// sr is a minimal SearchResult: an id and a relevance (cosine) score. RawScore>0
// marks it a genuine query hit (a candidate seed); RawScore==0 mimics a
// kgExpand-surfaced chunk (e.g. the low-cosine gold) that anchors on nothing but
// can be boosted by sharing entities with a seed.
func sr(id string, raw float64) domain.SearchResult {
	return domain.SearchResult{Document: domain.Document{ID: id}, RawScore: raw}
}

// TestChunkCoherence_IslandSinks: three conv-26 chunks share a `dated at`
// timestamp hub + a speaker entity; a conv-44 distractor shares neither. The
// island must score 0; the cluster chunks must score > 0.
func TestChunkCoherence_IslandSinks(t *testing.T) {
	st := newFakeChunkTripletsStore()
	for _, id := range []string{"a", "b", "c"} {
		_ = st.SaveChunkTriplets(context.Background(), id, []ChunkTriplet{
			{H: id, R: "dated at", T: "t26"},
			{H: "melanie", R: "read", T: "book"},
		})
	}
	_ = st.SaveChunkTriplets(context.Background(), "d", []ChunkTriplet{
		{H: "d", R: "dated at", T: "t44"},
		{H: "tim", R: "read", T: "newspaper"},
	})

	got := chunkCoherence(context.Background(), st,
		[]domain.SearchResult{sr("a", 0.9), sr("b", 0.8), sr("c", 0.7), sr("d", 0.6)}, 10)
	if got["d"] != 0 {
		t.Fatalf("cross-conversation island must score 0, got %f", got["d"])
	}
	for _, id := range []string{"a", "b", "c"} {
		if got[id] <= 0 {
			t.Fatalf("same-conversation cluster chunk %q must score > 0, got %f", id, got[id])
		}
	}
}

// TestChunkCoherence_SeedAnchoredBeatsSize is the regression test for the fix:
// the prior pool-wide version rewarded cluster SIZE, demoting a correct gold that
// sat in a lightly-retrieved session beneath a big irrelevant one. Seed-anchored
// coherence must instead boost the low-cosine gold (because a relevant seed sits
// in its session) and give the big irrelevant cluster ZERO (it has no seed).
func TestChunkCoherence_SeedAnchoredBeatsSize(t *testing.T) {
	st := newFakeChunkTripletsStore()
	// Relevant session: a high-cosine seed + the low-cosine gold, sharing the
	// session timestamp and a rare speaker entity.
	_ = st.SaveChunkTriplets(context.Background(), "seed", []ChunkTriplet{
		{H: "seed", R: "dated at", T: "t26"},
		{H: "melanie", R: "read", T: "book"},
	})
	_ = st.SaveChunkTriplets(context.Background(), "gold", []ChunkTriplet{
		{H: "gold", R: "dated at", T: "t26"},
		{H: "melanie", R: "recommended", T: "novel"},
	})
	// Big irrelevant session: five chunks, densely interlinked by their own
	// timestamp — but none is a query-relevant seed.
	big := []string{"x1", "x2", "x3", "x4", "x5"}
	for _, id := range big {
		_ = st.SaveChunkTriplets(context.Background(), id, []ChunkTriplet{
			{H: id, R: "dated at", T: "t99"},
			{H: "crowd", R: "at", T: "party"},
		})
	}

	results := []domain.SearchResult{
		sr("seed", 0.9), // the only genuine relevant hit
		sr("gold", 0.0), // kgExpand-surfaced, low cosine
	}
	for _, id := range big {
		results = append(results, sr(id, 0.1)) // weakly relevant at best
	}
	// seedN=1 ⇒ only the top hit ("seed") anchors the spread.
	got := chunkCoherence(context.Background(), st, results, 1)

	if got["gold"] <= 0 {
		t.Fatalf("low-cosine gold must be boosted via its link to the seed, got %f", got["gold"])
	}
	for _, id := range big {
		if got[id] != 0 {
			t.Fatalf("big irrelevant cluster %q must score 0 (no seed), got %f", id, got[id])
		}
	}
	if got["gold"] <= got["x1"] {
		t.Fatalf("gold (%f) must outrank the big-cluster distractor (%f)", got["gold"], got["x1"])
	}
}

// TestChunkCoherence_NoSignalCases covers the safe fallbacks: nil store, a single
// candidate, no relevant seed, and a disjoint pool all yield an empty map.
func TestChunkCoherence_NoSignalCases(t *testing.T) {
	if m := chunkCoherence(context.Background(), nil, []domain.SearchResult{sr("a", 1), sr("b", 1)}, 10); len(m) != 0 {
		t.Fatalf("nil store must yield no signal, got %v", m)
	}
	st := newFakeChunkTripletsStore()
	_ = st.SaveChunkTriplets(context.Background(), "a", []ChunkTriplet{{H: "x", R: "r", T: "y"}})
	if m := chunkCoherence(context.Background(), st, []domain.SearchResult{sr("a", 1)}, 10); len(m) != 0 {
		t.Fatalf("single candidate must yield no signal, got %v", m)
	}
	// Two candidates that share an entity but NEITHER is a relevant seed (rel=0).
	_ = st.SaveChunkTriplets(context.Background(), "b", []ChunkTriplet{{H: "x", R: "r", T: "y"}})
	if m := chunkCoherence(context.Background(), st, []domain.SearchResult{sr("a", 0), sr("b", 0)}, 10); len(m) != 0 {
		t.Fatalf("no relevant seed must yield no signal, got %v", m)
	}
	// Two relevant seeds that share nothing ⇒ max raw is 0 ⇒ empty map.
	_ = st.SaveChunkTriplets(context.Background(), "c", []ChunkTriplet{{H: "p", R: "r", T: "q"}})
	if m := chunkCoherence(context.Background(), st, []domain.SearchResult{sr("a", 1), sr("c", 1)}, 10); len(m) != 0 {
		t.Fatalf("disjoint seeds must yield no signal, got %v", m)
	}
}
