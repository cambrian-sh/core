package memory

import (
	"math"
	"testing"
)

func sumScores(m map[string]float64) float64 {
	var s float64
	for _, v := range m {
		s += v
	}
	return s
}

func approx(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestPageRank_Empty(t *testing.T) {
	if got := ComputePageRank(nil, PageRankParams{}); len(got) != 0 {
		t.Fatalf("empty input must yield empty map, got %v", got)
	}
}

func TestPageRank_SingleChunk(t *testing.T) {
	got := ComputePageRank([]ChunkEntities{{ChunkID: "a", Entities: []string{"x"}}}, PageRankParams{})
	if !approx(got["a"], 1.0, 1e-9) {
		t.Fatalf("single chunk must get all the mass, got %v", got)
	}
}

func TestPageRank_SumsToOne(t *testing.T) {
	chunks := []ChunkEntities{
		{ChunkID: "a", Entities: []string{"x", "y"}},
		{ChunkID: "b", Entities: []string{"y", "z"}},
		{ChunkID: "c", Entities: []string{"z"}},
		{ChunkID: "d", Entities: []string{"isolated"}}, // dangling
	}
	got := ComputePageRank(chunks, PageRankParams{})
	if !approx(sumScores(got), 1.0, 1e-6) {
		t.Fatalf("scores must sum to ~1, got %f (%v)", sumScores(got), got)
	}
	// The dangling node still gets at least the teleport baseline.
	base := (1 - 0.85) / 4
	if got["d"] < base-1e-9 {
		t.Fatalf("dangling node below teleport baseline: %f < %f", got["d"], base)
	}
}

func TestPageRank_StarHubRanksHighest(t *testing.T) {
	// Hub shares a DISTINCT entity with each spoke ⇒ hub has degree 3, spokes 1.
	chunks := []ChunkEntities{
		{ChunkID: "hub", Entities: []string{"a", "b", "c"}},
		{ChunkID: "s1", Entities: []string{"a"}},
		{ChunkID: "s2", Entities: []string{"b"}},
		{ChunkID: "s3", Entities: []string{"c"}},
	}
	got := ComputePageRank(chunks, PageRankParams{})
	for _, s := range []string{"s1", "s2", "s3"} {
		if got["hub"] <= got[s] {
			t.Fatalf("hub (%f) must outrank spoke %s (%f)", got["hub"], s, got[s])
		}
	}
	// Spokes are symmetric.
	if !approx(got["s1"], got["s2"], 1e-6) || !approx(got["s2"], got["s3"], 1e-6) {
		t.Fatalf("symmetric spokes must score equally: %v", got)
	}
}

func TestPageRank_DFCapDropsHubEntity(t *testing.T) {
	// "hub" entity is in ALL 5 chunks (df=5); "rare" only in a+b (df=2).
	chunks := []ChunkEntities{
		{ChunkID: "a", Entities: []string{"hub", "rare"}},
		{ChunkID: "b", Entities: []string{"hub", "rare"}},
		{ChunkID: "c", Entities: []string{"hub"}},
		{ChunkID: "d", Entities: []string{"hub"}},
		{ChunkID: "e", Entities: []string{"hub"}},
	}
	// With a df cap of 2, "hub" (df=5) is dropped → only the rare a-b edge
	// survives. c/d/e become isolated (they get only the teleport baseline +
	// their uniform share of redistributed dangling mass — equal to each other
	// and strictly below the connected rare-pair a/b).
	capped := ComputePageRank(chunks, PageRankParams{DFCap: 2})
	if !approx(capped["c"], capped["d"], 1e-6) || !approx(capped["d"], capped["e"], 1e-6) {
		t.Fatalf("isolated c/d/e should score equally under the cap: %v", capped)
	}
	if capped["a"] <= capped["c"] || !approx(capped["a"], capped["b"], 1e-6) {
		t.Fatalf("rare-pair a/b should outrank isolated c and equal each other: %v", capped)
	}

	// Without the cap, "hub" connects all 5 → c/d/e are no longer isolated; the
	// hub inflow lifts c's score above its capped (isolated) value. This proves
	// the cap is what dropped the hub edge set.
	uncapped := ComputePageRank(chunks, PageRankParams{DFCap: 0})
	if uncapped["c"] <= capped["c"]+1e-9 {
		t.Fatalf("without df-cap, hub should connect c (%f) above its capped value (%f)", uncapped["c"], capped["c"])
	}
}

func TestPageRank_DedupsAndStable(t *testing.T) {
	// Duplicate chunk id is ignored; result deterministic across runs.
	chunks := []ChunkEntities{
		{ChunkID: "a", Entities: []string{"x"}},
		{ChunkID: "a", Entities: []string{"y"}}, // dup id — ignored
		{ChunkID: "b", Entities: []string{"x"}},
	}
	g1 := ComputePageRank(chunks, PageRankParams{})
	g2 := ComputePageRank(chunks, PageRankParams{})
	if len(g1) != 2 {
		t.Fatalf("dup chunk id must collapse to 2 nodes, got %d", len(g1))
	}
	for k := range g1 {
		if !approx(g1[k], g2[k], 1e-12) {
			t.Fatalf("non-deterministic score for %s: %f vs %f", k, g1[k], g2[k])
		}
	}
}

func TestChunkEntitiesFromTriplets(t *testing.T) {
	rows := []struct{ ChunkID, Entity string }{
		{"a", "x"}, {"a", "y"}, {"a", "x"}, // dup entity deduped
		{"b", "y"},
		{"", "z"}, // blank chunk skipped
		{"c", ""}, // blank entity skipped
	}
	got := ChunkEntitiesFromTriplets(rows)
	if len(got) != 2 {
		t.Fatalf("expected 2 chunks (a,b), got %d: %v", len(got), got)
	}
	if got[0].ChunkID != "a" || len(got[0].Entities) != 2 {
		t.Fatalf("chunk a should have 2 deduped entities, got %v", got[0])
	}
}
