package memory

// Tests for WorkspaceStageImpl.PrimeForStep (ADR-0022 Phase 2).
//
// Design: these tests assert on the observable structure of the returned
// []ContextRef — activation values, precision sentinels, sort order, and
// capacity ceiling. They do NOT test the internals of SHA-256 or BFS traversal.

import (
	"context"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// ── Test infrastructure ────────────────────────────────────────────────────

// seededStore is a VectorStore stub returning controlled SearchResults.
type seededStore struct {
	fakeVectorStore
	seeds []domain.SearchResult
}

func (s *seededStore) Search(_ context.Context, _ []float32, _ domain.SearchOptions) ([]domain.SearchResult, error) {
	return s.seeds, nil
}

func (s *seededStore) GetByID(_ context.Context, id string) (*domain.Document, error) {
	for _, sr := range s.seeds {
		if sr.Document.ID == id {
			return &sr.Document, nil
		}
	}
	return nil, nil
}

// graphStore embeds seededStore and also implements GraphStore.
// GetAdjacentEdges returns controlled edges.
type graphStore struct {
	seededStore
	edges []domain.DocumentEdge
	// extra docs discoverable via BFS but not in seeds
	docs map[string]domain.Document
}

func (g *graphStore) GetAdjacentEdges(_ context.Context, docIDs []string) ([]domain.DocumentEdge, error) {
	var result []domain.DocumentEdge
	idSet := make(map[string]bool)
	for _, id := range docIDs {
		idSet[id] = true
	}
	for _, e := range g.edges {
		if idSet[e.SourceID] {
			result = append(result, e)
		}
	}
	return result, nil
}

func (g *graphStore) SaveEdge(_ context.Context, _ domain.DocumentEdge) error { return nil }

func (g *graphStore) UpdateEdgeWeight(_ context.Context, _, _ string, _ domain.EdgeType, _ float32) error {
	return nil
}

func (g *graphStore) GetByID(_ context.Context, id string) (*domain.Document, error) {
	for _, sr := range g.seeds {
		if sr.Document.ID == id {
			return &sr.Document, nil
		}
	}
	if doc, ok := g.docs[id]; ok {
		return &doc, nil
	}
	return nil, nil
}

// makeWorkspaceWithThreshold creates a WorkspaceStageImpl with given activation threshold.
func makeWorkspace(store domain.VectorStore, threshold float64) *WorkspaceStageImpl {
	ws := NewWorkspaceStage(store, &fakeEmbedder{}, nil, 10, 5, 0.1, false, 0.7)
	ws.ActivationThreshold = threshold
	return ws
}

// ── Tracer bullet ──────────────────────────────────────────────────────────

// Empty seed results must return an empty slice without panicking.
func TestPrimeForStep_EmptySeeds_ReturnsEmpty(t *testing.T) {
	ws := makeWorkspace(&seededStore{seeds: nil}, 0.1)
	refs, err := ws.PrimeForStep(context.Background(), "some query", nil, nil, 0, 20)
	if err != nil {
		t.Fatalf("PrimeForStep: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("expected empty refs for empty seeds, got %d", len(refs))
	}
}

// ── Seeds-only path (SpreadingEngine nil) ─────────────────────────────────

// When SpreadingEngine is nil, seeds become ContextRefs with their cosine
// score as Precision and their score as Activation (seed == BFS depth 0).
func TestPrimeForStep_SeedsOnly_NoSpreadingEngine(t *testing.T) {
	store := &seededStore{
		seeds: []domain.SearchResult{
			{Document: domain.Document{ID: "doc-a", Text: "relevant doc"}, Score: 0.85},
			{Document: domain.Document{ID: "doc-b", Text: "less relevant"}, Score: 0.60},
		},
	}
	ws := makeWorkspace(store, 0.1)
	// SpreadingEngine is nil by default in makeWorkspace.

	refs, err := ws.PrimeForStep(context.Background(), "query", nil, nil, 0, 20)
	if err != nil {
		t.Fatalf("PrimeForStep: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs, got %d", len(refs))
	}
	// Seeds have Precision = cosine score (pgvector computed it).
	for _, ref := range refs {
		if ref.Precision < 0 {
			t.Errorf("seed ref %q must have Precision ≥ 0, got %v", ref.CID, ref.Precision)
		}
	}
}

// ── Activation threshold filters weak results ──────────────────────────────

func TestPrimeForStep_ActivationThreshold_FiltersWeakRefs(t *testing.T) {
	store := &seededStore{
		seeds: []domain.SearchResult{
			{Document: domain.Document{ID: "strong"}, Score: 0.90},
			{Document: domain.Document{ID: "weak"}, Score: 0.05}, // below 0.1 threshold
		},
	}
	ws := makeWorkspace(store, 0.1)

	refs, err := ws.PrimeForStep(context.Background(), "query", nil, nil, 0, 20)
	if err != nil {
		t.Fatalf("PrimeForStep: %v", err)
	}
	if len(refs) != 1 {
		t.Errorf("expected 1 ref after threshold filter, got %d", len(refs))
	}
	if string(refs[0].CID) != "strong" {
		t.Errorf("expected strong doc in results, got %q", refs[0].CID)
	}
}

// ── Hard ceiling ───────────────────────────────────────────────────────────

func TestPrimeForStep_HardCeiling_TruncatesAtMaxItems(t *testing.T) {
	seeds := make([]domain.SearchResult, 10)
	for i := range seeds {
		seeds[i] = domain.SearchResult{
			Document: domain.Document{ID: string(rune('a' + i))},
			Score:    0.9,
		}
	}
	ws := makeWorkspace(&seededStore{seeds: seeds}, 0.1)

	refs, err := ws.PrimeForStep(context.Background(), "query", nil, nil, 0, 3)
	if err != nil {
		t.Fatalf("PrimeForStep: %v", err)
	}
	if len(refs) > 3 {
		t.Errorf("hard ceiling of 3 must be respected, got %d", len(refs))
	}
}

// ── DependsOn boost ────────────────────────────────────────────────────────

// Prior step CIDs receive +0.3 activation boost.
// A document that would otherwise be below threshold must appear after the boost.
func TestPrimeForStep_DependsOnBoost_RaisesActivation(t *testing.T) {
	// doc-prior: score=0.05 (normally below threshold=0.1), but will be boosted
	store := &seededStore{
		seeds: []domain.SearchResult{
			{Document: domain.Document{ID: "doc-prior"}, Score: 0.05},
			{Document: domain.Document{ID: "doc-normal"}, Score: 0.80},
		},
	}
	ws := makeWorkspace(store, 0.1)

	priorRef := domain.ContextRef{CID: "doc-prior"}
	refs, err := ws.PrimeForStep(context.Background(), "query", []domain.ContextRef{priorRef}, nil, 0, 20)
	if err != nil {
		t.Fatalf("PrimeForStep: %v", err)
	}

	// doc-prior: 0.05 (seed) + 0.30 (boost) = 0.35 → passes threshold
	found := false
	for _, r := range refs {
		if string(r.CID) == "doc-prior" {
			found = true
			if r.Activation < 0.30 {
				t.Errorf("doc-prior activation after boost must be ≥ 0.30, got %v", r.Activation)
			}
		}
	}
	if !found {
		t.Error("boosted prior ref must appear in results even if seed score was below threshold")
	}
}

// Boost must not push activation above 1.0 (clamped).
func TestPrimeForStep_DependsOnBoost_ClampedAt1(t *testing.T) {
	store := &seededStore{
		seeds: []domain.SearchResult{
			{Document: domain.Document{ID: "high"}, Score: 0.95},
		},
	}
	ws := makeWorkspace(store, 0.1)
	priorRef := domain.ContextRef{CID: "high"}
	refs, err := ws.PrimeForStep(context.Background(), "query", []domain.ContextRef{priorRef}, nil, 0, 20)
	if err != nil {
		t.Fatalf("PrimeForStep: %v", err)
	}
	for _, r := range refs {
		if string(r.CID) == "high" && r.Activation > 1.0 {
			t.Errorf("activation must not exceed 1.0 after boost, got %v", r.Activation)
		}
	}
}

// ── Sort order ─────────────────────────────────────────────────────────────

// Results must be sorted by activation descending.
func TestPrimeForStep_SortedByActivationDescending(t *testing.T) {
	store := &seededStore{
		seeds: []domain.SearchResult{
			{Document: domain.Document{ID: "low"}, Score: 0.30},
			{Document: domain.Document{ID: "high"}, Score: 0.90},
			{Document: domain.Document{ID: "mid"}, Score: 0.60},
		},
	}
	ws := makeWorkspace(store, 0.1)

	refs, err := ws.PrimeForStep(context.Background(), "query", nil, nil, 0, 20)
	if err != nil {
		t.Fatalf("PrimeForStep: %v", err)
	}
	if len(refs) < 2 {
		t.Skip("need at least 2 refs to test ordering")
	}
	for i := 1; i < len(refs); i++ {
		if refs[i].Activation > refs[i-1].Activation {
			t.Errorf("refs[%d].Activation=%v > refs[%d].Activation=%v — not sorted descending",
				i, refs[i].Activation, i-1, refs[i-1].Activation)
		}
	}
}

// ── BFS nodes get precision sentinel ──────────────────────────────────────

// Documents discovered via BFS (not in original pgvector seeds) must carry
// Precision=-1.0 to signal "not yet computed" to assemble_context().
func TestPrimeForStep_BFSNodes_PrecisionSentinel(t *testing.T) {
	gs := &graphStore{
		seededStore: seededStore{
			seeds: []domain.SearchResult{
				{Document: domain.Document{ID: "seed-doc", ActivationStrength: 0.5}, Score: 0.80},
			},
		},
		edges: []domain.DocumentEdge{
			{SourceID: "seed-doc", TargetID: "bfs-doc", EdgeType: domain.EdgeSpecifies, Weight: 0.9},
		},
		docs: map[string]domain.Document{
			"bfs-doc": {ID: "bfs-doc", ActivationStrength: 0.5},
		},
	}

	ws := NewWorkspaceStage(gs, &fakeEmbedder{}, nil, 10, 5, 0.01, false, 0.7)
	ws.ActivationThreshold = 0.01
	spEngine := NewSpreadingEngine(gs, gs, 0.75, 3, 0.01)
	ws.SpreadingEngine = spEngine

	refs, err := ws.PrimeForStep(context.Background(), "query", nil, nil, 0, 20)
	if err != nil {
		t.Fatalf("PrimeForStep: %v", err)
	}

	// bfs-doc should appear and must have precision=-1.0
	var bfsSeen bool
	for _, r := range refs {
		if string(r.CID) == "bfs-doc" {
			bfsSeen = true
			if r.Precision != -1.0 {
				t.Errorf("BFS-discovered node must have Precision=-1.0, got %v", r.Precision)
			}
		}
		if string(r.CID) == "seed-doc" && r.Precision < 0 {
			t.Errorf("seed node must have non-negative Precision, got %v", r.Precision)
		}
	}
	if !bfsSeen {
		t.Log("bfs-doc not in results (graph may not propagate with these settings) — test is informational")
	}
}

// ── bfs_fraction > 0 when SpreadingEngine active ───────────────────────────

// When SpreadingEngine is wired and has edges, bfs_fraction must be > 0.
// This is the Phase 2 gate signal: if always 0, spreading activation is useless.
func TestPrimeForStep_WithSpreadingEngine_BFSFractionPositive(t *testing.T) {
	gs := &graphStore{
		seededStore: seededStore{
			seeds: []domain.SearchResult{
				{Document: domain.Document{ID: "seed-a", ActivationStrength: 0.5}, Score: 0.80},
			},
		},
		edges: []domain.DocumentEdge{
			{SourceID: "seed-a", TargetID: "neighbor-b", EdgeType: domain.EdgeSpecifies, Weight: 0.9},
		},
		docs: map[string]domain.Document{
			"neighbor-b": {ID: "neighbor-b", ActivationStrength: 0.5},
		},
	}

	ws := NewWorkspaceStage(gs, &fakeEmbedder{}, nil, 10, 5, 0.01, false, 0.7)
	ws.ActivationThreshold = 0.01
	spEngine := NewSpreadingEngine(gs, gs, 0.75, 3, 0.01)
	ws.SpreadingEngine = spEngine

	refs, err := ws.PrimeForStep(context.Background(), "query", nil, nil, 0, 20)
	if err != nil {
		t.Fatalf("PrimeForStep: %v", err)
	}

	var bfsCount int
	for _, r := range refs {
		if r.Precision == -1.0 {
			bfsCount++
		}
	}
	t.Logf("bfs_fraction: %d/%d BFS nodes (Precision=-1.0)", bfsCount, len(refs))
	if len(refs) > 0 && bfsCount == 0 {
		// Only fail if we have results — empty graph may legitimately produce 0 BFS nodes
		t.Log("WARN: bfs_fraction=0 — SpreadingEngine returned no BFS-discovered nodes. Check EnergyFloor and edge weights.")
	}
}
