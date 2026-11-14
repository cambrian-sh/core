package domain_test

import (
	"math"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// Cycle 1 — Zero age: TemporalDecay returns baseStrength unchanged.
func TestTemporalDecay_ZeroAge(t *testing.T) {
	t.Parallel()
	now := time.Now()
	got := domain.TemporalDecay(0.8, now, 0.01, now)
	if math.Abs(got-0.8) > 1e-9 {
		t.Fatalf("expected 0.8, got %v", got)
	}
}

// Cycle 2 — One hour age with lambda=0.01: result is base * e^(-0.01).
func TestTemporalDecay_OneHour(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	now := time.Now()
	future := now.Add(time.Hour)
	got := domain.TemporalDecay(0.5, future, 0.01, now)
	if math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("expected 0.5 (clamped), got %v", got)
	}
}

// Cycle 4 — Zero base returns zero regardless of age or lambda.
func TestTemporalDecay_ZeroBase(t *testing.T) {
	t.Parallel()
	now := time.Now()
	last := now.Add(-1000 * time.Hour)
	got := domain.TemporalDecay(0, last, 0.01, now)
	if got != 0 {
		t.Fatalf("expected 0, got %v", got)
	}
}

// Cycle 5 — 24 hours with default lambda=0.01: effective activation decays notably.
func TestTemporalDecay_24Hours(t *testing.T) {
	t.Parallel()
	now := time.Now()
	last := now.Add(-24 * time.Hour)
	got := domain.TemporalDecay(1.0, last, 0.01, now)
	want := math.Exp(-0.01 * 24)
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("expected %.9f, got %.9f", want, got)
	}
}
