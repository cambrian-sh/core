package memory

// Tests for the PrimeForStep LRU caches and CacheInvalidator (ADR-0022 Phase 2B).
//
// All tests assert on observable behaviour:
//   - How many times the VectorStore/Embedder is called
//   - Whether results change after cache invalidation
//
// No test inspects the internal cache data structure.

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// countingStore records Search call count and returns fixed seeds.
type countingStore struct {
	fakeVectorStore
	searchCalls atomic.Int32
	seeds       []domain.SearchResult
}

func (c *countingStore) Search(_ context.Context, _ []float32, _ domain.SearchOptions) ([]domain.SearchResult, error) {
	c.searchCalls.Add(1)
	return c.seeds, nil
}

// countingEmbedder records Embed call count.
type countingEmbedder struct {
	embedCalls atomic.Int32
}

func (e *countingEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	e.embedCalls.Add(1)
	return []float32{0.1, 0.2, 0.3}, nil
}

// makeWorkspaceWithCache returns a WorkspaceStageImpl with LRU cache capacity set.
func makeWorkspaceWithCache(store *countingStore, embedder *countingEmbedder, capacity int) *WorkspaceStageImpl {
	ws := &WorkspaceStageImpl{
		Store:               store,
		Embedder:            embedder,
		RetrievalFloor:      0.1,
		ActivationThreshold: 0.01,
	}
	ws.initCaches(capacity)
	return ws
}

// ── Tracer bullet: cache hit skips Store.Search ────────────────────────────

func TestPrimeForStepCache_HitSkipsVectorStore(t *testing.T) {
	store := &countingStore{
		seeds: []domain.SearchResult{
			{Document: domain.Document{ID: "doc-1"}, Score: 0.9},
		},
	}
	embedder := &countingEmbedder{}
	ws := makeWorkspaceWithCache(store, embedder, 100)

	ctx := context.Background()

	// First call: cache miss → hits Store.Search
	_, err := ws.PrimeForStep(ctx, "identical query", nil, nil, 0, 20)
	if err != nil {
		t.Fatalf("first PrimeForStep: %v", err)
	}
	if store.searchCalls.Load() != 1 {
		t.Errorf("first call: expected 1 Search call, got %d", store.searchCalls.Load())
	}

	// Second call same query: cache hit → Store.Search NOT called again
	_, err = ws.PrimeForStep(ctx, "identical query", nil, nil, 0, 20)
	if err != nil {
		t.Fatalf("second PrimeForStep: %v", err)
	}
	if store.searchCalls.Load() != 1 {
		t.Errorf("cache hit: expected still 1 Search call, got %d", store.searchCalls.Load())
	}
}

// ── Cache invalidation after commitBatch ───────────────────────────────────

func TestPrimeForStepCache_InvalidationTriggersRefresh(t *testing.T) {
	store := &countingStore{
		seeds: []domain.SearchResult{
			{Document: domain.Document{ID: "doc-1"}, Score: 0.9},
		},
	}
	ws := makeWorkspaceWithCache(store, &countingEmbedder{}, 100)
	ctx := context.Background()

	// Warm the cache.
	if _, err := ws.PrimeForStep(ctx, "query", nil, nil, 0, 20); err != nil {
		t.Fatalf("warm: %v", err)
	}
	callsBefore := store.searchCalls.Load()

	// Invalidate (simulates Tier-2 commitBatch adding new documents to pgvector).
	ws.InvalidateContextRefCache()

	// Post-invalidation call must hit Store.Search again.
	if _, err := ws.PrimeForStep(ctx, "query", nil, nil, 0, 20); err != nil {
		t.Fatalf("post-invalidation: %v", err)
	}
	if store.searchCalls.Load() <= callsBefore {
		t.Errorf("after invalidation: expected Search to be called again, still at %d", store.searchCalls.Load())
	}
}

// ── Cache capacity eviction ────────────────────────────────────────────────

// When capacity is exceeded, the least-recently-used entry is evicted.
// The evicted query must miss on the next call (going back to Store.Search).
func TestPrimeForStepCache_CapacityEvictsLRU(t *testing.T) {
	store := &countingStore{
		seeds: []domain.SearchResult{
			{Document: domain.Document{ID: "doc-x"}, Score: 0.9},
		},
	}
	ws := makeWorkspaceWithCache(store, &countingEmbedder{}, 2) // capacity=2
	ctx := context.Background()

	// Warm cache with entries A and B (capacity=2, both fit).
	if _, err := ws.PrimeForStep(ctx, "query-A", nil, nil, 0, 20); err != nil {
		t.Fatalf("query-A: %v", err)
	}
	if _, err := ws.PrimeForStep(ctx, "query-B", nil, nil, 0, 20); err != nil {
		t.Fatalf("query-B: %v", err)
	}
	callsAfterWarm := store.searchCalls.Load() // should be 2

	// Add C: evicts A (LRU).
	if _, err := ws.PrimeForStep(ctx, "query-C", nil, nil, 0, 20); err != nil {
		t.Fatalf("query-C: %v", err)
	}

	// B is still cached — no new Search call.
	if _, err := ws.PrimeForStep(ctx, "query-B", nil, nil, 0, 20); err != nil {
		t.Fatalf("query-B reuse: %v", err)
	}
	if store.searchCalls.Load() != callsAfterWarm+1 { // only C caused a new call
		t.Errorf("expected %d Search calls (A,B,C), got %d", callsAfterWarm+1, store.searchCalls.Load())
	}

	// A was evicted — must trigger a new Search call.
	callsBefore := store.searchCalls.Load()
	if _, err := ws.PrimeForStep(ctx, "query-A", nil, nil, 0, 20); err != nil {
		t.Fatalf("query-A evicted: %v", err)
	}
	if store.searchCalls.Load() <= callsBefore {
		t.Errorf("evicted entry A must cause a new Search call, calls still at %d", store.searchCalls.Load())
	}
}

// ── Embedding cache hit ────────────────────────────────────────────────────

// Same query string must produce the same embedding vector — Embed is
// deterministic, so the result can be cached indefinitely (no TTL).
func TestPrimeForStepCache_EmbeddingCachedAcrossCalls(t *testing.T) {
	store := &countingStore{
		seeds: []domain.SearchResult{
			{Document: domain.Document{ID: "doc-1"}, Score: 0.9},
		},
	}
	embedder := &countingEmbedder{}
	ws := makeWorkspaceWithCache(store, embedder, 100)
	ctx := context.Background()

	// Call 1: embedder called once.
	if _, err := ws.PrimeForStep(ctx, "same query", nil, nil, 0, 20); err != nil {
		t.Fatalf("call 1: %v", err)
	}
	// Invalidate the ContextRef cache (forces re-fetch from Store).
	ws.InvalidateContextRefCache()
	// Call 2: ContextRef cache miss, but embedding cache hit.
	if _, err := ws.PrimeForStep(ctx, "same query", nil, nil, 0, 20); err != nil {
		t.Fatalf("call 2: %v", err)
	}
	if embedder.embedCalls.Load() != 1 {
		t.Errorf("Embed must be called only once for the same query across ContextRef cache misses, got %d", embedder.embedCalls.Load())
	}
}

// ── Different queries use separate cache entries ───────────────────────────

func TestPrimeForStepCache_DifferentQueriesSeparateEntries(t *testing.T) {
	store := &countingStore{
		seeds: []domain.SearchResult{
			{Document: domain.Document{ID: "doc-1"}, Score: 0.9},
		},
	}
	ws := makeWorkspaceWithCache(store, &countingEmbedder{}, 100)
	ctx := context.Background()

	if _, err := ws.PrimeForStep(ctx, "query-X", nil, nil, 0, 20); err != nil {
		t.Fatalf("X: %v", err)
	}
	if _, err := ws.PrimeForStep(ctx, "query-Y", nil, nil, 0, 20); err != nil {
		t.Fatalf("Y: %v", err)
	}
	if store.searchCalls.Load() != 2 {
		t.Errorf("different queries must each hit Store.Search: got %d calls, want 2", store.searchCalls.Load())
	}
}
