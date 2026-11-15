package memory

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ENG-432: a fact-reached chunk whose own embedding barely matches the query (the
// evidence is a short tail of unrelated text, 9a707b82) ranks below a distractor in
// Stage A although the lane matched its fact strongly. With
// execution.facts.lane_chunk_cosine_max the chunk's Stage-A cosine signal is
// max(own, matched fact cosine); off, Stage A is unchanged.

// queryVec is what fakeEmbedder returns for every query.
var queryVec = []float32{0.1, 0.2, 0.3}

func vecDoc(id string, v []float32) domain.Document {
	d := chunkOf(id, "doc-"+id, "t")
	d.Embedding = domain.Embedding{Vector: v}
	return d
}

func TestStageACosine_UnitBothStageAPaths(t *testing.T) {
	t.Parallel()
	for _, calibrated := range []bool{false, true} {
		for _, on := range []bool{false, true} {
			q := &QueryService{}
			bl := NewBlender(BlendWeights{Cosine: 0.4})
			q.EnableBlend(&bl, zeroSignals{})
			q.SetRankingV2(RankingV2{Calibrated: calibrated, FactLane: true, FactChunkCosineMax: on})
			ann := factLaneHits{}
			ann.add("evidence", 0.95, false, laneFact("evidence", "baked a croissant", "t"), "fact")
			ctx := withFactHits(context.Background(), ann)
			pool := []domain.SearchResult{
				{Document: vecDoc("distractor", []float32{0.3, 0.2, 0.1})}, // cosine ≈ 0.71 to the query
				{Document: vecDoc("evidence", []float32{3, 0, -1})},        // cosine 0: the evidence is a tail
			}
			got := q.applyStageABlend(ctx, pool, queryVec)
			first := got[0].Document.ID
			if on && first != "evidence" {
				t.Errorf("calibrated=%t on: the fact-reached chunk must rank first, got %v", calibrated, ids(got))
			}
			if !on && first == "evidence" {
				t.Errorf("calibrated=%t off: Stage A must rank on the chunk's own cosine, got %v", calibrated, ids(got))
			}
		}
	}
}

// End to end through QueryService.Search: dense returns the distractor; the fact lane
// reaches the evidence chunk by id through the enforcing read store; Stage A orders
// the window the caller gets.
func TestFactLaneCosineMax_SearchRanksTheFactReachedChunkFirst(t *testing.T) {
	t.Parallel()
	chunks := []domain.Document{
		vecDoc("distractor", []float32{0.3, 0.2, 0.1}), // cosine ≈ 0.71 to the query
		vecDoc("evidence", []float32{3, 0, -1}),        // cosine 0: the evidence is a tail
	}
	store := &rankedChunkStore{laneChunkStore: &laneChunkStore{docs: chunks}, dense: []string{"distractor"}}
	reader := &scoredFactReader{facts: []domain.FactHit{
		scoredFact("f1", "evidence", "doc-evidence", "baked a croissant", 0.95, "t"),
	}}
	run := func(on bool) []string {
		q := scopedLaneService(t, store, reader, &domain.TagPredicate{}, 0)
		bl := NewBlender(BlendWeights{Cosine: 0.4})
		q.EnableBlend(&bl, zeroSignals{})
		q.SetRankingV2(RankingV2{FactLane: true, FactLaneTopK: 20, FactChunkCosineMax: on})
		res, err := q.Search(context.Background(), "what did I bake", "agent")
		if err != nil {
			t.Fatal(err)
		}
		return ids(res)
	}
	off := run(false)
	if len(off) < 2 || off[0] != "distractor" {
		t.Fatalf("precondition (flag off): the distractor must rank first, got %v", off)
	}
	if on := run(true); len(on) < 2 || on[0] != "evidence" {
		t.Fatalf("lane_chunk_cosine_max: the fact-reached chunk must rank first, got %v", on)
	}
}
