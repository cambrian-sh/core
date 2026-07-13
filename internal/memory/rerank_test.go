package memory

import (
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

func mkDoc(id string, activation float64, lastAccessed time.Time) domain.Document {
	return domain.Document{
		ID:                 id,
		DocumentType:       domain.DocTypeMnemonicFact,
		ActivationStrength: activation,
		LastAccessedAt:     lastAccessed,
		Text:               "fact " + id,
	}
}

func TestReRankWithAssociative_TwoTriggerFormula(t *testing.T) {
	now := time.Now()
	fresh := mkDoc("d1", 0.8, now)
	stale := mkDoc("d2", 0.8, now.Add(-100*24*time.Hour))
	cands := []domain.SearchResult{
		{Document: fresh, Score: 0.5},
		{Document: stale, Score: 0.5},
	}
	reach := map[string]float64{"d1": 0.1, "d2": 0.1}
	got := ReRankWithAssociative(cands, reach, 0.005, 0.2, now)
	if len(got) != 2 {
		t.Fatalf("want 2, got %d", len(got))
	}
	if got[0].Document.ID != "d1" {
		t.Errorf("expected fresh doc to outrank stale, got order %s vs %s", got[0].Document.ID, got[1].Document.ID)
	}
}

func TestReRankWithAssociative_BoostsDocsWithReachability(t *testing.T) {
	now := time.Now()
	highReach := mkDoc("d1", 0.5, now)
	lowReach := mkDoc("d2", 0.5, now)
	cands := []domain.SearchResult{
		{Document: highReach, Score: 0.5},
		{Document: lowReach, Score: 0.55},
	}
	reach := map[string]float64{"d1": 1.0, "d2": 0.0}
	got := ReRankWithAssociative(cands, reach, 0.005, 0.5, now)
	if got[0].Document.ID != "d1" {
		t.Errorf("high-reachability doc should outrank slightly-higher-cosine; got %s first", got[0].Document.ID)
	}
}

func TestReRankWithAssociative_BetaZeroSkipsReachability(t *testing.T) {
	now := time.Now()
	a := mkDoc("a", 0.5, now)
	b := mkDoc("b", 0.5, now)
	cands := []domain.SearchResult{
		{Document: a, Score: 0.4},
		{Document: b, Score: 0.5},
	}
	// With beta=0, the reachability map is ignored and the formula reduces
	// to the temporal-decay re-rank.
	reach := map[string]float64{"a": 999}
	got := ReRankWithAssociative(cands, reach, 0.005, 0, now)
	if got[0].Document.ID != "b" {
		t.Errorf("with β=0, doc with higher cosine should win; got %s", got[0].Document.ID)
	}
}

func TestReRankWithAssociative_NegativeBetaClampsToZero(t *testing.T) {
	now := time.Now()
	cands := []domain.SearchResult{{Document: mkDoc("a", 0.5, now), Score: 0.4}}
	got := ReRankWithAssociative(cands, map[string]float64{"a": 1.0}, 0.005, -1, now)
	if got[0].Score <= 0 {
		t.Errorf("negative beta should clamp to 0, not negate the score; got %f", got[0].Score)
	}
}

func TestReRankWithAssociative_EmptyReachability(t *testing.T) {
	now := time.Now()
	a := mkDoc("a", 0.5, now)
	b := mkDoc("b", 0.5, now)
	cands := []domain.SearchResult{
		{Document: a, Score: 0.3},
		{Document: b, Score: 0.7},
	}
	got := ReRankWithAssociative(cands, nil, 0.005, 0.2, now)
	if got[0].Document.ID != "b" {
		t.Errorf("no reachability → fall back to cosine order; got %s first", got[0].Document.ID)
	}
}

func TestReRankWithAssociative_InputNotMutated(t *testing.T) {
	now := time.Now()
	cands := []domain.SearchResult{
		{Document: mkDoc("a", 0.5, now), Score: 0.3},
		{Document: mkDoc("b", 0.5, now), Score: 0.7},
	}
	origFirst := cands[0].Document.ID
	_ = ReRankWithAssociative(cands, nil, 0.005, 0.2, now)
	if cands[0].Document.ID != origFirst {
		t.Errorf("input should not be mutated")
	}
}

func TestComputeReachability_EmptyInputs(t *testing.T) {
	if got := ComputeReachability(nil, NewEntityIndex(), []float32{1, 0}); len(got) != 0 {
		t.Errorf("empty candidates should return empty map")
	}
	got := ComputeReachability([]domain.SearchResult{{Document: mkDoc("d", 0.5, time.Now())}}, nil, []float32{1, 0})
	if got == nil {
		t.Errorf("nil index should return empty (not nil) map")
	}
	if len(got) != 0 {
		t.Errorf("nil index should return empty map, got %+v", got)
	}
	if got := ComputeReachability([]domain.SearchResult{{Document: mkDoc("d", 0.5, time.Now())}}, NewEntityIndex(), nil); len(got) != 0 {
		t.Errorf("nil query should return empty map")
	}
}

func TestComputeReachability_AggregatesEntityCosine(t *testing.T) {
	idx := NewEntityIndex()
	idx.SetNameEmbedding("named:caroline", "Caroline", domain.Embedding{Vector: []float32{1, 0, 0}})
	idx.SetNameEmbedding("concept:adoption", "adoption", domain.Embedding{Vector: []float32{0.9, 0.1, 0}})
	idx.Add("named:caroline", "d1", 0.9, MetaKindNamed, 1)
	idx.Add("concept:adoption", "d1", 0.7, MetaKindConcept, 1)
	idx.Add("named:caroline", "d2", 0.5, MetaKindNamed, 1)

	cands := []domain.SearchResult{
		{Document: mkDoc("d1", 0.5, time.Now())},
		{Document: mkDoc("d2", 0.5, time.Now())},
	}
	query := []float32{1, 0, 0}
	reach := ComputeReachability(cands, idx, query)

	r1, ok := reach["d1"]
	if !ok || r1 <= 0 {
		t.Errorf("d1 should have positive reachability, got %v", r1)
	}
	// d2 should have a smaller reachability (shares only one entity with d1
	// at a lower weight).
	r2, ok := reach["d2"]
	if !ok {
		t.Errorf("d2 should have reachability too, got %v", reach)
	}
	if r1 < r2 {
		t.Errorf("d1 (both entities) should outrank d2 (one entity) on reachability: d1=%f d2=%f", r1, r2)
	}
}

func TestComputeReachability_NoEmbeddingNoReachability(t *testing.T) {
	idx := NewEntityIndex()
	// Add an entity WITHOUT a stored embedding.
	idx.Add("named:nobody", "d1", 0.5, MetaKindNamed, 1)
	cands := []domain.SearchResult{{Document: mkDoc("d1", 0.5, time.Now())}}
	reach := ComputeReachability(cands, idx, []float32{1, 0})
	if _, ok := reach["d1"]; ok {
		t.Errorf("entity without embedding should produce no reachability, got %+v", reach)
	}
}
