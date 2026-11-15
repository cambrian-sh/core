package memory

import (
	"context"
	"log/slog"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// ENG-83: the anchor, section and spreading lanes read the chunks they bring in
// with ONE GetBatch on the enforcing store per lane (per BFS level for spreading),
// not one GetByID per id. countingStore counts the round trips.
type countingStore struct {
	*vecStoreWithDocs
	byID, batch atomic.Int32
}

func (c *countingStore) GetByID(ctx context.Context, id string) (*domain.Document, error) {
	c.byID.Add(1)
	return c.vecStoreWithDocs.GetByID(ctx, id)
}

func (c *countingStore) GetBatch(ctx context.Context, ids []string) ([]domain.Document, error) {
	c.batch.Add(1)
	return c.vecStoreWithDocs.GetBatch(ctx, ids)
}

func TestSpreadingEngine_OneBatchReadPerLevel(t *testing.T) {
	t.Parallel()
	now := time.Now()
	gs := &recordingGraphStore{edges: []domain.DocumentEdge{
		{SourceID: "seed", TargetID: "a", EdgeType: domain.EdgeSpecifies, Weight: 0.9, CreatedAt: now},
		{SourceID: "seed", TargetID: "b", EdgeType: domain.EdgeSpecifies, Weight: 0.9, CreatedAt: now},
		{SourceID: "seed", TargetID: "c", EdgeType: domain.EdgeSpecifies, Weight: 0.9, CreatedAt: now},
	}}
	docs := map[string]domain.Document{}
	for _, id := range []string{"a", "b", "c"} {
		docs[id] = domain.Document{ID: id, ActivationStrength: 1}
	}
	cs := &countingStore{vecStoreWithDocs: &vecStoreWithDocs{docs: docs}}
	se := NewSpreadingEngine(gs, authz.NewEnforcingVectorStore(cs, slog.Default()), 0.75, 1, 0.05)
	ctx := domain.WithScope(context.Background(), domain.ScopeSystem)
	got := se.Spread(ctx, []domain.SearchResult{{Document: domain.Document{ID: "seed", ActivationStrength: 1}, Score: 1}})
	if len(got) != 4 {
		t.Fatalf("expansions = %d, want seed + 3 targets", len(got))
	}
	if cs.byID.Load() != 0 || cs.batch.Load() != 1 {
		t.Fatalf("round trips: GetByID=%d GetBatch=%d, want 0 and 1 for one BFS level", cs.byID.Load(), cs.batch.Load())
	}
}

// ENG-1683: equal-energy expansions come back in a stable order (seeds, then BFS
// discovery order), never Go map order.
func TestSpreadingEngine_ResultOrderIsDeterministic(t *testing.T) {
	t.Parallel()
	now := time.Now()
	var edges []domain.DocumentEdge
	docs := map[string]domain.Document{}
	var want []string
	for _, id := range []string{"t1", "t2", "t3", "t4", "t5", "t6", "t7", "t8"} {
		edges = append(edges, domain.DocumentEdge{SourceID: "seed", TargetID: id, EdgeType: domain.EdgeSpecifies, Weight: 0.9, CreatedAt: now})
		docs[id] = domain.Document{ID: id, ActivationStrength: 1}
		want = append(want, id)
	}
	se := NewSpreadingEngine(&recordingGraphStore{edges: edges}, &vecStoreWithDocs{docs: docs}, 0.75, 1, 0.05)
	want = append([]string{"seed"}, want...)
	for run := 0; run < 50; run++ {
		got := se.Spread(context.Background(), []domain.SearchResult{{Document: domain.Document{ID: "seed", ActivationStrength: 1}, Score: 1}})
		var order []string
		for _, e := range got {
			order = append(order, e.Document.ID)
		}
		if !reflect.DeepEqual(order, want) {
			t.Fatalf("run %d: order %v, want %v", run, order, want)
		}
	}
}

func TestPromotionLanes_OneBatchReadForMissingIDs(t *testing.T) {
	t.Parallel()
	st := newFakeChunkTripletsStore()
	_ = st.SaveChunkTriplets(context.Background(), "g1", []domain.ChunkTriplet{{H: "g1", R: "has_anchor", T: "chapter:1"}})
	_ = st.SaveChunkTriplets(context.Background(), "g2", []domain.ChunkTriplet{{H: "g2", R: "has_anchor", T: "chapter:1"}})
	cs := &countingStore{vecStoreWithDocs: &vecStoreWithDocs{docs: map[string]domain.Document{"g1": {ID: "g1"}, "g2": {ID: "g2"}}}}
	q := &QueryService{chunkTriplets: st, vectorStore: cs, anchorConstraint: true, kgPerEntity: 5}
	out := q.applyAnchorConstraint(context.Background(), nil, "What happens in Chapter 1?", nil, &domain.TagPredicate{Bypass: true})
	if len(out) != 2 {
		t.Fatalf("both anchored chunks must be promoted, got %v", ids(out))
	}
	if cs.byID.Load() != 0 || cs.batch.Load() != 1 {
		t.Fatalf("round trips: GetByID=%d GetBatch=%d, want 0 and 1", cs.byID.Load(), cs.batch.Load())
	}
}
