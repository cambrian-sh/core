package centralexec

import (
	"context"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/internal/centralexec/belief"
	"github.com/cambrian-sh/cambrian-runtime/domain"
)

func vsRegions() []domain.CapabilityRegion {
	return []domain.CapabilityRegion{
		{Label: "comparison", Centroid: []float32{1, 0}},
		{Label: "summarization", Centroid: []float32{0, 1}},
	}
}

func vsConfig() belief.Config {
	return belief.Config{PriorExpectedSuccess: 0.5, FastAlpha: 0.5, SlowAlpha: 0.1, ConfidenceK: 5, MinSimilarity: 0.5}
}

// The Verifier signal feeds the belief store: a high-quality outcome on an
// intent raises that resource's precision in the matching region (ADR-0037 D8,
// 0037-04 #3). The Verifier emits a magnitude, not a leaderboard score.
func TestVerifierConsolidator_DrivesRegionUpdate(t *testing.T) {
	store := belief.New(vsRegions(), vsConfig())
	store.SeedPrior("agent")
	cons := &VerifierConsolidator{Updater: store, InhibitionThreshold: 0.3}

	emb := []float32{1, 0} // comparison
	for i := 0; i < 5; i++ {
		if _, err := cons.Consume(context.Background(), "agent", emb, 1.0); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	if got := store.Belief("agent", emb).ExpectedSuccess; got <= 0.5 {
		t.Errorf("comparison ExpectedSuccess = %v, want raised above prior by Verifier signal", got)
	}
	if got := store.Belief("agent", []float32{0, 1}).ExpectedSuccess; got != 0.5 {
		t.Errorf("summarization ExpectedSuccess = %v, want still 0.5 (region-specific)", got)
	}
}

// A single below-threshold run fires fast-path inhibition (one-run eviction /
// Surveillance) — the safety valve for the offline-consolidation lag (D8, #4).
func TestVerifierConsolidator_FastPathInhibition(t *testing.T) {
	store := belief.New(vsRegions(), vsConfig())
	store.SeedPrior("flaky")
	cons := &VerifierConsolidator{Updater: store, InhibitionThreshold: 0.3}

	inhibited, err := cons.Consume(context.Background(), "flaky", []float32{1, 0}, 0.1) // below threshold
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !inhibited {
		t.Error("a below-threshold single run must fire fast-path inhibition")
	}

	good, _ := cons.Consume(context.Background(), "flaky", []float32{1, 0}, 0.9)
	if good {
		t.Error("a healthy run must NOT fire fast-path inhibition")
	}
}
