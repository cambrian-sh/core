package postgres

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/cambrian-sh/core/domain"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvector "github.com/pgvector/pgvector-go/pgx"
)

// newTestAdapter creates a PgVectorAdapter connected to the test database.
// Requires PG_TEST_DSN env var (e.g., "host=localhost port=5432 user=postgres password=postgres dbname=testdb sslmode=disable").
// Returns the adapter and a cleanup function. Caller must defer cleanup().
func newTestAdapter(t *testing.T) (*PgVectorAdapter, func()) {
	t.Helper()

	dsn := os.Getenv("PG_TEST_DSN")
	if dsn == "" {
		t.Skip("PG_TEST_DSN not set; skipping integration test that requires PostgreSQL")
	}

	pgxCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("failed to parse PG_TEST_DSN: %v", err)
	}
	pgxCfg.MaxConns = 5
	pgxCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return pgxvector.RegisterTypes(ctx, conn)
	}

	pool, err := pgxpool.NewWithConfig(context.Background(), pgxCfg)
	if err != nil {
		t.Fatalf("failed to create pgx pool: %v", err)
	}

	adapter := &PgVectorAdapter{pool: pool, dim: eventTestDim}
	if err := adapter.ensureSchema(context.Background()); err != nil {
		pool.Close()
		t.Fatalf("ensureSchema failed: %v", err)
	}

	// Post-0007 (ADR-0093) the vectorised rows the adapter saves and searches live
	// in `chunks`; `documents` is the entity table and never held these rows.
	// Deleting from `documents` here was a silent no-op that let `test-%` chunks
	// leak across tests — TestRetrievalFloor_Guarantee then found an AS=0.8 row
	// from TestFloorMultiplier and scored 0.84 instead of ~0.2
	// (T-20261031-four-core-postgres-adapter-tests-fail-against).
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, "DELETE FROM chunks WHERE id LIKE 'test-%'")
		pool.Close()
	})

	return adapter, func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, "DELETE FROM chunks WHERE id LIKE 'test-%'")
		pool.Close()
	}
}

func testCtx() context.Context {
	return context.Background()
}

func dummyVec() []float32 {
	v := make([]float32, eventTestDim)
	v[0] = 0.1
	return v
}

func dummyEmb() domain.Embedding {
	return domain.Embedding{Vector: dummyVec()}
}

// Cycle 1: UpdateActivationStrength increases activation_strength by the given delta.
func TestUpdateActivationStrength_IncrementsValue(t *testing.T) {
	adapter, cleanup := newTestAdapter(t)
	defer cleanup()

	ctx := testCtx()
	doc := &domain.Document{
		ID:                 "test-incr-001",
		Text:               "test document for activation increment",
		DocumentType:       domain.DocTypeMemory,
		Embedding:          dummyEmb(),
		ActivationStrength: 0.1,
	}
	if err := adapter.Save(ctx, doc); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	if err := adapter.UpdateActivationStrength(ctx, "test-incr-001", 0.05); err != nil {
		t.Fatalf("UpdateActivationStrength failed: %v", err)
	}

	retrieved, err := adapter.GetByID(ctx, "test-incr-001")
	if err != nil {
		t.Fatalf("GetByID failed: %v", err)
	}
	if retrieved == nil {
		t.Fatal("GetByID returned nil")
	}
	if retrieved.ActivationStrength < 0.145 || retrieved.ActivationStrength > 0.155 {
		t.Errorf("ActivationStrength after +0.05 = %v, want approximately 0.15", retrieved.ActivationStrength)
	}
}

// Cycle 2: UpdateActivationStrength plates at the maturation ceiling (0.8).
func TestUpdateActivationStrength_PlatesAtCeiling(t *testing.T) {
	adapter, cleanup := newTestAdapter(t)
	defer cleanup()

	ctx := testCtx()
	doc := &domain.Document{
		ID:                 "test-plateau-001",
		Text:               "test document for plateau check",
		DocumentType:       domain.DocTypeMemory,
		Embedding:          dummyEmb(),
		ActivationStrength: 0.75,
	}
	if err := adapter.Save(ctx, doc); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	for i := 0; i < 15; i++ {
		if err := adapter.UpdateActivationStrength(ctx, "test-plateau-001", 0.05); err != nil {
			t.Fatalf("UpdateActivationStrength call %d failed: %v", i, err)
		}
	}

	retrieved, err := adapter.GetByID(ctx, "test-plateau-001")
	if err != nil {
		t.Fatalf("GetByID failed: %v", err)
	}
	if retrieved == nil {
		t.Fatal("GetByID returned nil")
	}
	if retrieved.ActivationStrength > 0.8+1e-9 {
		t.Errorf("ActivationStrength after 15 bumps = %v, want ≤ 0.8 (maturation ceiling)", retrieved.ActivationStrength)
	}
	if retrieved.ActivationStrength < 0.79 {
		t.Errorf("ActivationStrength after 15 bumps = %v, want approximately 0.8", retrieved.ActivationStrength)
	}
}

// Cycle 3: Activation strength never exceeds 1.0 (global cap).
func TestUpdateActivationStrength_NeverExceedsOne(t *testing.T) {
	adapter, cleanup := newTestAdapter(t)
	defer cleanup()

	ctx := testCtx()
	doc := &domain.Document{
		ID:                 "test-cap-001",
		Text:               "test document for global cap check",
		DocumentType:       domain.DocTypeMemory,
		Embedding:          dummyEmb(),
		ActivationStrength: 0.95,
	}
	if err := adapter.Save(ctx, doc); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	if err := adapter.UpdateActivationStrength(ctx, "test-cap-001", 0.1); err != nil {
		t.Fatalf("UpdateActivationStrength failed: %v", err)
	}

	retrieved, err := adapter.GetByID(ctx, "test-cap-001")
	if err != nil {
		t.Fatalf("GetByID failed: %v", err)
	}
	if retrieved == nil {
		t.Fatal("GetByID returned nil")
	}
	if retrieved.ActivationStrength > 1.0+1e-9 {
		t.Errorf("ActivationStrength = %v, want ≤ 1.0", retrieved.ActivationStrength)
	}
}

// Cycle 4: IncrementAccess bumps activation_strength by +0.05.
func TestIncrementAccess_BumpsActivationStrength(t *testing.T) {
	adapter, cleanup := newTestAdapter(t)
	defer cleanup()

	ctx := testCtx()
	doc := &domain.Document{
		ID:                 "test-acc-001",
		Text:               "test document for increment access",
		DocumentType:       domain.DocTypeMemory,
		Embedding:          dummyEmb(),
		ActivationStrength: 0.2,
	}
	if err := adapter.Save(ctx, doc); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	if err := adapter.IncrementAccess(ctx, "test-acc-001"); err != nil {
		t.Fatalf("IncrementAccess failed: %v", err)
	}

	retrieved, err := adapter.GetByID(ctx, "test-acc-001")
	if err != nil {
		t.Fatalf("GetByID failed: %v", err)
	}
	if retrieved == nil {
		t.Fatal("GetByID returned nil")
	}
	if retrieved.ActivationStrength < 0.245 || retrieved.ActivationStrength > 0.255 {
		t.Errorf("ActivationStrength after IncrementAccess = %v, want approximately 0.25", retrieved.ActivationStrength)
	}
	if retrieved.AccessCount != 1 {
		t.Errorf("AccessCount after IncrementAccess = %d, want 1", retrieved.AccessCount)
	}
}

// Cycle 5: Floor-multiplier re-ranking — high-AS document outranks high-cosine document.
func TestFloorMultiplier_Reranking(t *testing.T) {
	adapter, cleanup := newTestAdapter(t)
	defer cleanup()

	ctx := testCtx()

	// Create a shared vector; both docs point to it but we want different raw cosine scores.
	// We use two different vectors to get different raw cosine distances (and hence scores).
	docs := []*domain.Document{
		{
			ID:           "test-rerank-001",
			Text:         "doc with high activation but medium similarity",
			DocumentType: domain.DocTypeMemory,
			Embedding: domain.Embedding{Vector: func() []float32 {
				v := make([]float32, eventTestDim)
				v[0] = 0.3
				return v
			}()},
			ActivationStrength: 0.8,
		},
		{
			ID:           "test-rerank-002",
			Text:         "doc with low activation but high similarity",
			DocumentType: domain.DocTypeMemory,
			Embedding: domain.Embedding{Vector: func() []float32 {
				v := make([]float32, eventTestDim)
				v[0] = 0.85
				return v
			}()},
			ActivationStrength: 0.1,
		},
	}
	for _, d := range docs {
		if err := adapter.Save(ctx, d); err != nil {
			t.Fatalf("Save %s failed: %v", d.ID, err)
		}
	}

	// Query between both vectors — doc-002 should have higher raw cosine but lower re-ranked score.
	queryVec := make([]float32, eventTestDim)
	queryVec[0] = 0.5

	results, err := adapter.Search(ctx, queryVec, domain.SearchOptions{
		DocumentType:   domain.DocTypeMemory,
		TopK:           2,
		RetrievalFloor: 0.2,
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("Search returned %d results, want ≥ 2", len(results))
	}

	// High-AS (0.8) doc should be first due to floor-multiplier boosting.
	firstAs := results[0].Document.ActivationStrength
	secondAs := results[1].Document.ActivationStrength
	if firstAs < secondAs {
		t.Errorf("re-ranked order: first AS=%.2f, second AS=%.2f; want high-AS first", firstAs, secondAs)
	}
	t.Logf("floor-multiplier scores: [0]=%.4f (AS=%.2f), [1]=%.4f (AS=%.2f)",
		results[0].Score, results[0].Document.ActivationStrength,
		results[1].Score, results[1].Document.ActivationStrength)
}

// Cycle 6: Floor guarantee — with RetrievalFloor=0.2, AS=0 doc with cosine=0.99 scores ~0.2.
func TestRetrievalFloor_Guarantee(t *testing.T) {
	adapter, cleanup := newTestAdapter(t)
	defer cleanup()

	ctx := testCtx()
	v := make([]float32, eventTestDim)
	v[0] = 0.5

	doc := &domain.Document{
		ID:                 "test-floor-001",
		Text:               "floor guarantee test",
		DocumentType:       domain.DocTypeMemory,
		Embedding:          domain.Embedding{Vector: v},
		ActivationStrength: 0.0,
	}
	if err := adapter.Save(ctx, doc); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	queryVec := make([]float32, eventTestDim)
	queryVec[0] = 0.5

	results, err := adapter.Search(ctx, queryVec, domain.SearchOptions{
		DocumentType:   domain.DocTypeMemory,
		TopK:           1,
		RetrievalFloor: 0.2,
	})
	if err != nil {
		t.Fatalf("Search failed: %v", err)
	}
	if len(results) < 1 {
		t.Fatal("no results")
	}
	// Raw cosine ≈ 1.0, re-ranked = 1.0 × (0.2 + 0.8×0) = 0.2
	if results[0].Score < 0.15 || results[0].Score > 0.25 {
		t.Errorf("Score = %.4f, want approximately 0.2 (floor guarantee)", results[0].Score)
	}
	t.Logf("floor guarantee score: %.4f (AS=%.2f)", results[0].Score, results[0].Document.ActivationStrength)
}

// Cycle 7: Exploration rate — approximately 5% of returned slots carry exploration_slot=true.
func TestExplorationRate_Slots(t *testing.T) {
	adapter, cleanup := newTestAdapter(t)
	defer cleanup()

	ctx := testCtx()

	// Seed 150 documents so there's a tail beyond TopK for exploration sampling.
	for i := 0; i < 150; i++ {
		v := make([]float32, eventTestDim)
		v[0] = float32(i) * 0.006
		doc := &domain.Document{
			ID:                 fmt.Sprintf("test-explore-%02d", i),
			Text:               fmt.Sprintf("exploration doc %d", i),
			DocumentType:       domain.DocTypeMemory,
			Embedding:          domain.Embedding{Vector: v},
			ActivationStrength: 0.5,
		}
		if err := adapter.Save(ctx, doc); err != nil {
			t.Fatalf("Save failed: %v", err)
		}
	}

	// Run 50 queries with TopK=10; count exploration slots.
	// At 5% exploration: ceil(10*0.05)=1 slot per query → ~10% of slots are exploratory.
	exploreCount := 0
	totalResults := 0
	queryVec := make([]float32, eventTestDim)
	queryVec[0] = 0.5

	for q := 0; q < 50; q++ {
		results, err := adapter.Search(ctx, queryVec, domain.SearchOptions{
			DocumentType:    domain.DocTypeMemory,
			TopK:            10,
			RetrievalFloor:  0.2,
			ExplorationRate: 0.05,
		})
		if err != nil {
			t.Fatalf("Search failed: %v", err)
		}
		for _, r := range results {
			totalResults++
			if val, ok := r.Document.Metadata["exploration_slot"].(bool); ok && val {
				exploreCount++
			}
		}
	}

	rate := float64(exploreCount) / float64(totalResults)
	t.Logf("exploration rate: %d / %d = %.3f", exploreCount, totalResults, rate)
	// ceil(10*0.05)=1 slot per 10 → 10% expected rate, with ±6% tolerance.
	if rate < 0.02 || rate > 0.15 {
		t.Errorf("exploration rate = %.3f, want approximately 0.10 (±6%%)", rate)
	}
}
