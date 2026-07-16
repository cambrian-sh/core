package calibration

import (
	"math"
	"testing"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// PAVA must return a non-decreasing curve and pool monotonicity violations.
func TestFitIsotonic_MonotoneAndPooled(t *testing.T) {
	// A clear violation: x=2 has higher y than x=3 → they must pool to the mean.
	xs := []float64{1, 2, 3, 4}
	ys := []float64{0.1, 0.9, 0.2, 0.95}
	iso := FitIsotonic(xs, ys)
	// Non-decreasing outputs.
	for i := 1; i < len(iso.ys); i++ {
		if iso.ys[i] < iso.ys[i-1]-1e-9 {
			t.Fatalf("curve not monotone at %d: %v", i, iso.ys)
		}
	}
	// x=2 and x=3 pooled to (0.9+0.2)/2 = 0.55.
	if !approx(iso.Predict(2), 0.55) || !approx(iso.Predict(3), 0.55) {
		t.Fatalf("expected pooled 0.55 at x=2,3, got %v / %v", iso.Predict(2), iso.Predict(3))
	}
}

// Already-monotone data is preserved exactly.
func TestFitIsotonic_PreservesMonotone(t *testing.T) {
	iso := FitIsotonic([]float64{0.1, 0.5, 0.9}, []float64{0.2, 0.5, 0.8})
	if !approx(iso.Predict(0.1), 0.2) || !approx(iso.Predict(0.9), 0.8) {
		t.Fatalf("monotone data should be preserved, got %v..%v", iso.Predict(0.1), iso.Predict(0.9))
	}
	// Interpolation at the midpoint.
	if got := iso.Predict(0.3); got <= 0.2 || got >= 0.5 {
		t.Fatalf("interpolation at 0.3 out of range: %v", got)
	}
}

// A nil / empty curve is the identity.
func TestIsotonic_NilIsIdentity(t *testing.T) {
	var iso *Isotonic
	if got := iso.Predict(0.7); got != 0.7 {
		t.Fatalf("nil isotonic should be identity, got %v", got)
	}
	if FitIsotonic(nil, nil) != nil {
		t.Fatal("FitIsotonic(nil) should be nil")
	}
}

// Calibration corrects a uniformly-overconfident agent toward its verified quality.
func TestModel_CalibratesOverconfidentAgent(t *testing.T) {
	// Agent A always bids ~0.9 but verifies ~0.4 (overconfident); agent B bids 0.9 and
	// verifies 0.85 (calibrated). With enough samples, A's 0.9 should map far below B's.
	var samples []Sample
	for i := 0; i < 20; i++ {
		samples = append(samples, Sample{AgentID: "A", Confidence: 0.9, Quality: 0.4})
		samples = append(samples, Sample{AgentID: "B", Confidence: 0.9, Quality: 0.85})
	}
	m := Fit(samples, 10)
	ca := m.Calibrate("A", 0.9)
	cb := m.Calibrate("B", 0.9)
	if ca >= cb {
		t.Fatalf("overconfident A (%.3f) should calibrate below B (%.3f)", ca, cb)
	}
	if ca > 0.6 {
		t.Fatalf("A's 0.9 should map near its verified 0.4, got %.3f", ca)
	}
}

// Shrinkage: an agent with few samples is pulled toward the global prior.
func TestModel_ShrinkageTowardGlobal(t *testing.T) {
	var samples []Sample
	// Fleet-global: 0.9 → ~0.8 (many agents verify well).
	for i := 0; i < 50; i++ {
		samples = append(samples, Sample{AgentID: "good" + string(rune('a'+i%5)), Confidence: 0.9, Quality: 0.8})
	}
	// A single noisy sample for a rare agent claiming 0.9 but verifying 0.1.
	samples = append(samples, Sample{AgentID: "rare", Confidence: 0.9, Quality: 0.1})
	m := Fit(samples, 10)
	c := m.Calibrate("rare", 0.9)
	// With n=1 and minSamples=10, weight on the rare curve is 0.1 → mostly global (~0.8).
	if c < 0.6 {
		t.Fatalf("rare agent (n=1) should shrink toward the global ~0.8, got %.3f", c)
	}
}

// A nil model is the identity (offline-first: no model ⇒ no change).
func TestModel_NilIsIdentity(t *testing.T) {
	var m *Model
	if got := m.Calibrate("x", 0.42); got != 0.42 {
		t.Fatalf("nil model should be identity, got %v", got)
	}
}
