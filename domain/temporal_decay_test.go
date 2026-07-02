package domain_test

import (
	"math"
	"testing"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// Cycle 1 — Zero age: TemporalDecay returns baseStrength unchanged.
func TestTemporalDecay_ZeroAge(t *testing.T) {
	now := time.Now()
	got := domain.TemporalDecay(0.8, now, 0.01, now)
	if math.Abs(got-0.8) > 1e-9 {
		t.Fatalf("expected 0.8, got %v", got)
	}
}

// Cycle 2 — One hour age with lambda=0.01: result is base * e^(-0.01).
func TestTemporalDecay_OneHour(t *testing.T) {
	now := time.Now()
	last := now.Add(-time.Hour)
	want := 0.8 * math.Exp(-0.01)
	got := domain.TemporalDecay(0.8, last, 0.01, now)
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("expected %.9f, got %.9f", want, got)
	}
}

// Cycle 3 — Future lastAccessed clamps to zero age (no negative decay).
func TestTemporalDecay_FutureLastAccessed(t *testing.T) {
	now := time.Now()
	future := now.Add(time.Hour)
	got := domain.TemporalDecay(0.5, future, 0.01, now)
	if math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("expected 0.5 (clamped), got %v", got)
	}
}

// Cycle 4 — Zero base returns zero regardless of age or lambda.
func TestTemporalDecay_ZeroBase(t *testing.T) {
	now := time.Now()
	last := now.Add(-1000 * time.Hour)
	got := domain.TemporalDecay(0, last, 0.01, now)
	if got != 0 {
		t.Fatalf("expected 0, got %v", got)
	}
}

// Cycle 5 — 24 hours with default lambda=0.01: effective activation decays notably.
func TestTemporalDecay_24Hours(t *testing.T) {
	now := time.Now()
	last := now.Add(-24 * time.Hour)
	got := domain.TemporalDecay(1.0, last, 0.01, now)
	want := math.Exp(-0.01 * 24)
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("expected %.9f, got %.9f", want, got)
	}
}

// Cycle 6 — Ordering: three documents with same base but different lastAccessed
// are ranked A > B > C after re-rank (most recently accessed first).
func TestReRankWithTemporalDecay_OrdersByRecency(t *testing.T) {
	now := time.Now()
	lambda := 0.01

	candidates := []domain.SearchResult{
		{Document: domain.Document{ID: "A", ActivationStrength: 0.8, LastAccessedAt: now.Add(-1 * time.Hour)}, Score: 0.9},
		{Document: domain.Document{ID: "B", ActivationStrength: 0.8, LastAccessedAt: now.Add(-24 * time.Hour)}, Score: 0.9},
		{Document: domain.Document{ID: "C", ActivationStrength: 0.8, LastAccessedAt: now.Add(-168 * time.Hour)}, Score: 0.9},
	}

	ranked := domain.ReRankWithTemporalDecay(candidates, lambda, now)

	if len(ranked) != 3 {
		t.Fatalf("expected 3 results, got %d", len(ranked))
	}
	if ranked[0].Document.ID != "A" {
		t.Errorf("expected A first, got %s", ranked[0].Document.ID)
	}
	if ranked[1].Document.ID != "B" {
		t.Errorf("expected B second, got %s", ranked[1].Document.ID)
	}
	if ranked[2].Document.ID != "C" {
		t.Errorf("expected C third, got %s", ranked[2].Document.ID)
	}
}

// Cycle 7 — Re-rank preserves all results (no elements dropped).
func TestReRankWithTemporalDecay_AllResultsPreserved(t *testing.T) {
	now := time.Now()
	candidates := make([]domain.SearchResult, 10)
	for i := range candidates {
		candidates[i] = domain.SearchResult{
			Document: domain.Document{ID: string(rune('A' + i)), ActivationStrength: 0.5,
				LastAccessedAt: now.Add(-time.Duration(i) * time.Hour)},
			Score: 0.8,
		}
	}

	ranked := domain.ReRankWithTemporalDecay(candidates, 0.01, now)
	if len(ranked) != 10 {
		t.Fatalf("expected 10 results, got %d", len(ranked))
	}
}
