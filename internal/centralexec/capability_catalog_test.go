package centralexec

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// fakeRegionSource stands in for the CapabilityBelief store (0037-03). It yields
// raw regions; the catalog is responsible for the credible-mass projection.
type fakeRegionSource struct {
	regions []domain.CapabilityRegion
}

func (f fakeRegionSource) Regions(_ context.Context) ([]domain.CapabilityRegion, error) {
	return f.regions, nil
}

// Tracer (ADR-0037 D4, 0037-02 #1): the catalog is a pure projection — only
// regions with credible belief mass appear; empty-mass regions are dropped.
func TestCapabilityCatalog_ProjectsOnlyCredibleRegions(t *testing.T) {
	src := fakeRegionSource{regions: []domain.CapabilityRegion{
		{Label: "summarization", Centroid: []float32{1, 0}, BeliefMass: 0.7, SampleCount: 12},
		{Label: "interpretive-dance", Centroid: []float32{0, 1}, BeliefMass: 0.0, SampleCount: 0},
	}}
	cat := &CapabilityCatalog{Source: src, MinBeliefMass: 0.2}

	regions, err := cat.Regions(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(regions) != 1 {
		t.Fatalf("len(regions) = %d, want 1 (credible only)", len(regions))
	}
	if regions[0].Label != "summarization" {
		t.Errorf("region = %q, want summarization", regions[0].Label)
	}
}

// Reachability is the impossible-step guard (ADR-0037 D4, 0037-02 #3): an
// intent embedding near a credible region is reachable; an intent with no
// credible region nearby is structurally unreachable at generation time.
func TestCapabilityCatalog_Reachable(t *testing.T) {
	src := fakeRegionSource{regions: []domain.CapabilityRegion{
		{Label: "summarization", Centroid: []float32{1, 0}, BeliefMass: 0.7, SampleCount: 12},
		// "translation" has a centroid but NO credible mass — must not make
		// an intent reachable.
		{Label: "translation", Centroid: []float32{0, 1}, BeliefMass: 0.0, SampleCount: 0},
	}}
	cat := &CapabilityCatalog{Source: src, MinBeliefMass: 0.2, MinSimilarity: 0.8}

	// Near the summarization centroid → reachable.
	region, ok, err := cat.Reachable(context.Background(), []float32{0.99, 0.05})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok || region.Label != "summarization" {
		t.Errorf("Reachable = (%q, %v), want (summarization, true)", region.Label, ok)
	}

	// Near the translation centroid, but translation has no credible mass →
	// unreachable (the impossible step).
	_, ok, err = cat.Reachable(context.Background(), []float32{0.05, 0.99})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("intent landing only in an empty-mass region must be unreachable")
	}
}

// The catalog exposes its labels as the planner's drafting vocabulary
// (0037-02 #2: drafted in the existing embedding space, cluster names as labels).
func TestCapabilityCatalog_Vocabulary(t *testing.T) {
	src := fakeRegionSource{regions: []domain.CapabilityRegion{
		{Label: "summarization", BeliefMass: 0.7},
		{Label: "analysis", BeliefMass: 0.5},
		{Label: "empty", BeliefMass: 0.0},
	}}
	cat := &CapabilityCatalog{Source: src, MinBeliefMass: 0.2}

	vocab, err := cat.Vocabulary(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vocab) != 2 {
		t.Fatalf("vocab = %v, want 2 credible labels", vocab)
	}
}
