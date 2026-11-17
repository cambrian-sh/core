package memory

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0049 D10: co-activated pairs are the combinations of the top-N results above
// the co-activation floor.
func TestCoActivatedPairs(t *testing.T) {
	results := []domain.SearchResult{
		{Document: domain.Document{ID: "a"}, Score: 0.9},
		{Document: domain.Document{ID: "b"}, Score: 0.7},
		{Document: domain.Document{ID: "c"}, Score: 0.3}, // below floor → excluded
		{Document: domain.Document{ID: "d"}, Score: 0.6},
	}
	if pairs := coActivatedPairs(results, 0.5, 5); len(pairs) != 3 {
		t.Fatalf("expected 3 pairs from {a,b,d}; got %d (%v)", len(pairs), pairs)
	}
	if pairs := coActivatedPairs(results, 0.5, 2); len(pairs) != 1 {
		t.Errorf("topN=2 caps to {a,b} → 1 pair; got %d", len(pairs))
	}
	if pairs := coActivatedPairs(results, 0.95, 5); len(pairs) != 0 {
		t.Errorf("nothing above 0.95 → no pairs; got %d", len(pairs))
	}
}

// ADR-0049 D10: reinforcement decays by age, adds the learning rate, caps at max.
func TestReinforcedWeight(t *testing.T) {
	q := &QueryService{heb: hebbianParams{lr: 0.05, max: 0.9, decayPerDay: 0.95}}
	now := time.Now()
	approx := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

	if w := q.reinforcedWeight(0.2, time.Time{}, now); !approx(w, 0.25) {
		t.Errorf("fresh edge: 0.2 + lr → 0.25; got %v", w)
	}
	if w := q.reinforcedWeight(0.89, time.Time{}, now); !approx(w, 0.9) {
		t.Errorf("cap: clamps to max 0.9; got %v", w)
	}
	old := now.Add(-10 * 24 * time.Hour)
	if w := q.reinforcedWeight(0.5, old, now); w >= 0.55 {
		t.Errorf("decay: a 10-day-old edge decays before +lr; got %v", w)
	}
}

// ADR-0049 D10: the spreader decays a co_activated edge's stored weight by age on
// read (decay-on-spread-read); structural edges and disabled-decay are untouched.
func TestSpreadingEngine_CoActivatedWeightDecaysOnRead(t *testing.T) {
	s := &SpreadingEngine{HebbianDecayPerDay: 0.9}
	now := time.Now()
	approx := func(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

	if w := s.coActivatedWeight(domain.DocumentEdge{Weight: 0.5, CreatedAt: now}); !approx(w, 0.5) {
		t.Errorf("fresh edge → ~stored weight 0.5; got %v", w)
	}
	old := domain.DocumentEdge{Weight: 0.5, CreatedAt: now.Add(-10 * 24 * time.Hour)}
	if w := s.coActivatedWeight(old); w >= 0.5 {
		t.Errorf("10-day-old edge must decay below 0.5; got %v", w)
	}
	if w := (&SpreadingEngine{HebbianDecayPerDay: 0}).coActivatedWeight(old); w != 0.5 {
		t.Errorf("decay disabled → stored weight unchanged; got %v", w)
	}
	if w := s.coActivatedWeight(domain.DocumentEdge{Weight: 0.5}); w != 0.5 {
		t.Errorf("zero CreatedAt → no decay; got %v", w)
	}
}

// A recall's strongly co-retrieved docs get co_activated edges written (both dirs).
func TestReinforceCoActivation_WritesEdges(t *testing.T) {
	gs := &captureGraphStore{}
	q := NewQueryService(&fakeEmbedder{}, &scopeApplyingStore{}, nil)
	q.EnableHebbian(gs, 0.05, 0.9, 0.5, 0.95, 0.2, 5)

	q.reinforcePairs(context.Background(), coActivatedPairs([]domain.SearchResult{
		{Document: domain.Document{ID: "a"}, Score: 0.9},
		{Document: domain.Document{ID: "b"}, Score: 0.8},
	}, q.heb.floor, q.heb.topN))

	if len(gs.edges) != 2 {
		t.Fatalf("expected 2 co_activated edges (both directions); got %d", len(gs.edges))
	}
	for _, e := range gs.edges {
		if e.EdgeType != domain.EdgeCoActivated || e.Weight != 0.2 {
			t.Errorf("new co_activated edge should start at base 0.2; got %+v", e)
		}
	}
}

// waitHebbian blocks until every in-flight async reinforcement has finished, by
// taking all of the semaphore's slots and giving them back.
func waitHebbian(q *QueryService) {
	for i := 0; i < cap(q.hebInflight); i++ {
		q.hebInflight <- struct{}{}
	}
	for i := 0; i < cap(q.hebInflight); i++ {
		<-q.hebInflight
	}
}

// ENG-1951: the recall path's async reinforcement must reinforce exactly the
// co-retrieved set and share nothing with the rest of searchByType. It used to close
// over the variable `filtered`, which the neighbour window, document completion and
// annotateFactHits reassign after the goroutine starts — a data race under -race
// (this test failed there), and a reinforcement of whatever rows `filtered` held by
// then.
func TestSearch_HebbianReinforcesTheCoRetrievedSetWithoutRacing(t *testing.T) {
	gs := &captureGraphStore{}
	store := &scopeApplyingStore{docs: []domain.Document{
		{ID: "a", Text: "alpha"}, {ID: "b", Text: "beta"}, {ID: "c", Text: "gamma"},
	}}
	q := NewQueryService(&fakeEmbedder{}, store, nil)
	q.EnableNeighborWindow()
	// floor 0: every returned row is "hot", so the pairs are all of {a,b,c}.
	q.EnableHebbian(gs, 0.05, 0.9, 0, 0.95, 0.2, 5)

	const recalls = 8
	for i := 0; i < recalls; i++ {
		got, err := q.SearchSystem(context.Background(), "what")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) < 2 {
			t.Fatalf("fixture must recall >= 2 rows for Hebbian to fire; got %d", len(got))
		}
	}
	waitHebbian(q)

	edges := gs.snapshot()
	if len(edges) == 0 {
		t.Fatal("no co_activated edges written: the async reinforcement never ran")
	}
	want := map[string]bool{"a|b": true, "a|c": true, "b|c": true}
	seen := map[string]bool{}
	for _, e := range edges {
		k := hebbianEdgeKey(e.SourceID, e.TargetID)
		if e.EdgeType != domain.EdgeCoActivated || !want[k] {
			t.Fatalf("edge outside the co-retrieved set {a,b,c}: %+v", e)
		}
		seen[e.SourceID+">"+e.TargetID] = true
	}
	if len(seen) != 6 {
		t.Errorf("want both directions of all 3 pairs (6 directed edges); got %d: %v", len(seen), seen)
	}
}
