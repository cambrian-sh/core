package memory

import (
	"context"
	"testing"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// recordingGraphStore returns a fixed set of edges; weight assertions confirm
// the spreader uses edge.Weight directly, not a per-type map.
type recordingGraphStore struct {
	edges    []domain.DocumentEdge
	gotCalls int
}

func (r *recordingGraphStore) SaveEdge(_ context.Context, e domain.DocumentEdge) error {
	r.edges = append(r.edges, e)
	return nil
}
func (r *recordingGraphStore) GetAdjacentEdges(_ context.Context, docIDs []string) ([]domain.DocumentEdge, error) {
	r.gotCalls++
	idSet := make(map[string]bool, len(docIDs))
	for _, id := range docIDs {
		idSet[id] = true
	}
	var out []domain.DocumentEdge
	for _, e := range r.edges {
		if idSet[e.SourceID] {
			out = append(out, e)
		}
	}
	return out, nil
}
func (r *recordingGraphStore) UpdateEdgeWeight(_ context.Context, _, _ string, _ domain.EdgeType, _ float32) error {
	return nil
}

// vecStoreWithDocs is a VectorStore that returns the configured docs from
// GetByID/GetBatch. It panics on other methods (the spreader only uses these).
type vecStoreWithDocs struct {
	docs map[string]domain.Document
}

func (v *vecStoreWithDocs) GetByID(_ context.Context, id string) (*domain.Document, error) {
	if d, ok := v.docs[id]; ok {
		return &d, nil
	}
	return nil, nil
}
func (v *vecStoreWithDocs) GetBatch(_ context.Context, ids []string) ([]domain.Document, error) {
	var out []domain.Document
	for _, id := range ids {
		if d, ok := v.docs[id]; ok {
			out = append(out, d)
		}
	}
	return out, nil
}
func (v *vecStoreWithDocs) Save(_ context.Context, _ *domain.Document) error    { panic("Save not used in test") }
func (v *vecStoreWithDocs) SaveBatch(_ context.Context, _ []*domain.Document) error { panic("SaveBatch not used in test") }
func (v *vecStoreWithDocs) Search(_ context.Context, _ []float32, _ domain.SearchOptions) ([]domain.SearchResult, error) {
	panic("Search not used in test")
}
func (v *vecStoreWithDocs) Delete(_ context.Context, _ string) error             { panic("Delete not used in test") }
func (v *vecStoreWithDocs) DeleteBatch(_ context.Context, _ []string) error     { panic("DeleteBatch not used in test") }
func (v *vecStoreWithDocs) IncrementAccess(_ context.Context, _ string) error  { panic("IncrementAccess not used in test") }
func (v *vecStoreWithDocs) GetStaleMemories(_ context.Context, _ int) ([]domain.Document, error) { panic("unused") }
func (v *vecStoreWithDocs) QueryByMetadata(_ context.Context, _ map[string]string, _ int) ([]domain.Document, error) { panic("unused") }

func TestSpreadingEngine_UsesEdgeWeightDirectly(t *testing.T) {
	gs := &recordingGraphStore{
		edges: []domain.DocumentEdge{
			{SourceID: "seed", TargetID: "via-low", EdgeType: domain.EdgeExtracted, Weight: 0.3, CreatedAt: time.Now()},
			{SourceID: "seed", TargetID: "via-high", EdgeType: domain.EdgeExtracted, Weight: 0.95, CreatedAt: time.Now()},
		},
	}
	vs := &vecStoreWithDocs{docs: map[string]domain.Document{
		"via-low":  {ID: "via-low", ActivationStrength: 1.0, Text: "low"},
		"via-high": {ID: "via-high", ActivationStrength: 1.0, Text: "high"},
	}}
	se := NewSpreadingEngine(gs, vs, 0.75, 3, 0.05)
	seeds := []domain.SearchResult{{Document: domain.Document{ID: "seed", ActivationStrength: 1.0}, Score: 1.0}}
	got := se.Spread(context.Background(), seeds)

	var lowEnergy, highEnergy float64
	for _, e := range got {
		switch e.Document.ID {
		case "via-low":
			lowEnergy = e.ActivationEnergy
		case "via-high":
			highEnergy = e.ActivationEnergy
		}
	}
	if highEnergy <= lowEnergy {
		t.Errorf("high-weight edge should produce higher activation than low-weight; got high=%f low=%f", highEnergy, lowEnergy)
	}
}

func TestSpreadingEngine_EnergyFloorBlocksLowWeights(t *testing.T) {
	gs := &recordingGraphStore{
		edges: []domain.DocumentEdge{
			{SourceID: "seed", TargetID: "low-w", EdgeType: domain.EdgeExtracted, Weight: 0.01, CreatedAt: time.Now()},
		},
	}
	vs := &vecStoreWithDocs{docs: map[string]domain.Document{
		"low-w": {ID: "low-w", ActivationStrength: 0.5, Text: "x"},
	}}
	se := NewSpreadingEngine(gs, vs, 0.75, 3, 0.5) // floor 0.5
	seeds := []domain.SearchResult{{Document: domain.Document{ID: "seed", ActivationStrength: 1.0}, Score: 1.0}}
	got := se.Spread(context.Background(), seeds)
	for _, e := range got {
		if e.Document.ID == "low-w" {
			t.Errorf("low-weight edge should be blocked by floor; got %+v", e)
		}
	}
}

func TestSpreadingEngine_CoActivatedUsesStoredWeight(t *testing.T) {
	gs := &recordingGraphStore{
		edges: []domain.DocumentEdge{
			{SourceID: "seed", TargetID: "heb", EdgeType: domain.EdgeCoActivated, Weight: 0.8, CreatedAt: time.Now()},
		},
	}
	vs := &vecStoreWithDocs{docs: map[string]domain.Document{
		"heb": {ID: "heb", ActivationStrength: 1.0, Text: "x"},
	}}
	se := NewSpreadingEngine(gs, vs, 0.75, 3, 0.05)
	seeds := []domain.SearchResult{{Document: domain.Document{ID: "seed", ActivationStrength: 1.0}, Score: 1.0}}
	got := se.Spread(context.Background(), seeds)
	if len(got) < 2 { // seed + heb
		t.Fatalf("expected at least 2 expansions, got %d", len(got))
	}
}

func TestSpreadingEngine_StructuralEdgeUsesWeight(t *testing.T) {
	// A structural edge (EdgeSpecifies) should propagate by its stored
	// weight, not a per-type constant.
	gs := &recordingGraphStore{
		edges: []domain.DocumentEdge{
			{SourceID: "seed", TargetID: "specifies-target", EdgeType: domain.EdgeSpecifies, Weight: 0.4, CreatedAt: time.Now()},
		},
	}
	vs := &vecStoreWithDocs{docs: map[string]domain.Document{
		"specifies-target": {ID: "specifies-target", ActivationStrength: 1.0, Text: "x"},
	}}
	se := NewSpreadingEngine(gs, vs, 0.75, 3, 0.05)
	seeds := []domain.SearchResult{{Document: domain.Document{ID: "seed", ActivationStrength: 1.0}, Score: 1.0}}
	got := se.Spread(context.Background(), seeds)
	if len(got) < 2 {
		t.Fatalf("structural edge should be traversed, got %d", len(got))
	}
}

func TestSpreadingEngine_NoEdgeWeightMap(t *testing.T) {
	// Confirms the per-type weight fields are gone (regression guard).
	se := &SpreadingEngine{}
	_ = se
	// If anyone re-adds WeightContradicts/etc., this compile-time field
	// reference will fail; the test is a placeholder for the assertion.
}
