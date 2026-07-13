package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ── test doubles ─────────────────────────────────────────────────────────────

// docTypeRouter routes Search calls to per-type result slices.
// It embeds fakeVectorStore to satisfy all other VectorStore methods.
type docTypeRouter struct {
	fakeVectorStore
	byType map[string]routedResult
}

type routedResult struct {
	docs []domain.SearchResult
	err  error
}

func (d *docTypeRouter) Search(_ context.Context, _ []float32, opts domain.SearchOptions) ([]domain.SearchResult, error) {
	if r, ok := d.byType[opts.DocumentType]; ok {
		return r.docs, r.err
	}
	return nil, nil
}

func (d *docTypeRouter) GetBatch(_ context.Context, _ []string) ([]domain.Document, error) {
	return nil, nil
}

// staticPolicyProvider returns a fixed "episodic" policy.
type staticEpisodicPP struct {
	threshold float64
	topK      int
}

func (s *staticEpisodicPP) GetPolicy(name string) (domain.HippocampusPolicy, bool) {
	if name == "episodic" {
		return domain.HippocampusPolicy{
			SimilarityThreshold: s.threshold,
			MaxAgeHours:         8760,
		}, true
	}
	return domain.HippocampusPolicy{}, false
}

func (s *staticEpisodicPP) DefaultPolicy() domain.HippocampusPolicy {
	return domain.HippocampusPolicy{SimilarityThreshold: 0.65}
}

func episodicDoc(score float64) domain.SearchResult {
	return domain.SearchResult{
		Document: domain.Document{
			ID:           "ep-1",
			DocumentType: domain.DocTypeEpisodicMemory,
			Text:         "auth design: use JWT; skip refresh tokens",
			Metadata:     map[string]interface{}{"episodic": map[string]interface{}{"session_id": "sess-ep1"}},
		},
		Score: score,
	}
}

func newPolicyStage(store domain.VectorStore, threshold float64, topK int) *WorkspaceStageImpl {
	ws := NewWorkspaceStage(store, &fakeEmbedder{}, nil, 10, 5, 0.2, false, 0.7)
	ws.PolicyProvider = &staticEpisodicPP{threshold: threshold, topK: topK}
	return ws
}

// ── Cycle 1: episodic hit above threshold appears in LTMEnrichment.Episodes ──

func TestPrimeForPlanning_EpisodicHit_AboveThreshold(t *testing.T) {
	store := &docTypeRouter{
		byType: map[string]routedResult{
			domain.DocTypeMnemonicFact:   {docs: factDocs(2)},
			domain.DocTypeNegativeEdge:   {docs: nil},
			domain.DocTypeEpisodicMemory: {docs: []domain.SearchResult{episodicDoc(0.70)}},
		},
	}
	ws := newPolicyStage(store, 0.65, 3)

	result, err := ws.PrimeForPlanning(context.Background(), "auth design decisions")
	if err != nil {
		t.Fatalf("PrimeForPlanning: %v", err)
	}
	if len(result.Episodes) != 1 {
		t.Errorf("Episodes: want 1 got %d", len(result.Episodes))
	}
	if result.Episodes[0].Score != 0.70 {
		t.Errorf("Episodes[0].Score: want 0.70 got %v", result.Episodes[0].Score)
	}
}

// ── Cycle 2: episodic hit below threshold is excluded ────────────────────────

func TestPrimeForPlanning_EpisodicHit_BelowThreshold(t *testing.T) {
	store := &docTypeRouter{
		byType: map[string]routedResult{
			domain.DocTypeMnemonicFact:   {docs: factDocs(2)},
			domain.DocTypeNegativeEdge:   {docs: nil},
			domain.DocTypeEpisodicMemory: {docs: []domain.SearchResult{episodicDoc(0.50)}},
		},
	}
	ws := newPolicyStage(store, 0.65, 3)

	result, err := ws.PrimeForPlanning(context.Background(), "unrelated query")
	if err != nil {
		t.Fatalf("PrimeForPlanning: %v", err)
	}
	if len(result.Episodes) != 0 {
		t.Errorf("Episodes: want 0 (below threshold) got %d", len(result.Episodes))
	}
}

// ── Cycle 3: episodic search error leaves Facts and Negatives intact ─────────

func TestPrimeForPlanning_EpisodicError_DoesNotAffectOtherLanes(t *testing.T) {
	store := &docTypeRouter{
		byType: map[string]routedResult{
			domain.DocTypeMnemonicFact:   {docs: factDocs(2)},
			domain.DocTypeNegativeEdge:   {docs: []domain.SearchResult{{Document: domain.Document{ID: "neg-1"}}}},
			domain.DocTypeEpisodicMemory: {err: errors.New("pgvector timeout")},
		},
	}
	ws := newPolicyStage(store, 0.65, 3)

	result, err := ws.PrimeForPlanning(context.Background(), "some query")
	if err != nil {
		t.Fatalf("PrimeForPlanning: %v", err)
	}
	if len(result.Facts) != 2 {
		t.Errorf("Facts: want 2 got %d (episodic error must not affect Facts)", len(result.Facts))
	}
	if len(result.Negatives) != 1 {
		t.Errorf("Negatives: want 1 got %d (episodic error must not affect Negatives)", len(result.Negatives))
	}
	if len(result.Episodes) != 0 {
		t.Errorf("Episodes: want 0 on error, got %d", len(result.Episodes))
	}
}

// ── Cycle 4: no PolicyProvider → episodic lane skipped, no panic ─────────────

func TestPrimeForPlanning_NoPolicyProvider_EpisodesNil(t *testing.T) {
	store := &docTypeRouter{
		byType: map[string]routedResult{
			domain.DocTypeMnemonicFact:   {docs: factDocs(1)},
			domain.DocTypeNegativeEdge:   {docs: nil},
			domain.DocTypeEpisodicMemory: {docs: []domain.SearchResult{episodicDoc(0.99)}},
		},
	}
	// No PolicyProvider set
	ws := NewWorkspaceStage(store, &fakeEmbedder{}, nil, 10, 5, 0.2, false, 0.7)

	result, err := ws.PrimeForPlanning(context.Background(), "some query")
	if err != nil {
		t.Fatalf("PrimeForPlanning: %v", err)
	}
	if len(result.Episodes) != 0 {
		t.Errorf("Episodes: want 0 when no PolicyProvider, got %d", len(result.Episodes))
	}
}

// ── Cycle 5: empty episodic corpus → empty Episodes, no panic ────────────────

func TestPrimeForPlanning_EmptyEpisodicCorpus_NoEpisodes(t *testing.T) {
	store := &docTypeRouter{
		byType: map[string]routedResult{
			domain.DocTypeMnemonicFact:   {docs: factDocs(1)},
			domain.DocTypeNegativeEdge:   {docs: nil},
			domain.DocTypeEpisodicMemory: {docs: nil},
		},
	}
	ws := newPolicyStage(store, 0.65, 3)

	result, err := ws.PrimeForPlanning(context.Background(), "some query")
	if err != nil {
		t.Fatalf("PrimeForPlanning: %v", err)
	}
	if len(result.Episodes) != 0 {
		t.Errorf("Episodes: want 0 for empty corpus, got %d", len(result.Episodes))
	}
}

// ── Cycle 6: existing Facts and Negatives lanes unaffected when episodic is active ──

func TestPrimeForPlanning_ExistingLanes_UnaffectedByEpisodicLane(t *testing.T) {
	store := &docTypeRouter{
		byType: map[string]routedResult{
			domain.DocTypeMnemonicFact:   {docs: factDocs(3)},
			domain.DocTypeNegativeEdge:   {docs: []domain.SearchResult{{Document: domain.Document{ID: "neg-1"}}}},
			domain.DocTypeEpisodicMemory: {docs: []domain.SearchResult{episodicDoc(0.80)}},
		},
	}
	ws := newPolicyStage(store, 0.65, 3)

	result, err := ws.PrimeForPlanning(context.Background(), "design query")
	if err != nil {
		t.Fatalf("PrimeForPlanning: %v", err)
	}
	if len(result.Facts) != 3 {
		t.Errorf("Facts: want 3 got %d", len(result.Facts))
	}
	if len(result.Negatives) != 1 {
		t.Errorf("Negatives: want 1 got %d", len(result.Negatives))
	}
	if len(result.Episodes) != 1 {
		t.Errorf("Episodes: want 1 got %d", len(result.Episodes))
	}
}
