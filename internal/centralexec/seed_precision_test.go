package centralexec

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// The static seed makes every candidate routable immediately at low confidence
// — the cold-start prior (ADR-0037 D2) the foundation slice uses before any
// learned belief exists (0037-01: "precision can be a static seed").
func TestSeedPrecisionProvider_RoutableAtLowConfidence(t *testing.T) {
	p := SeedPrecisionProvider{SeedExpectedSuccess: 0.5, SeedConfidence: 0.1}
	candidates := []domain.AgentDefinition{{ID: "fresh-a"}, {ID: "fresh-b"}}

	weights, err := p.PrecisionFor(context.Background(), domain.Intent{ID: "i1"}, candidates)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(weights) != 2 {
		t.Fatalf("len(weights) = %d, want 2", len(weights))
	}
	for _, w := range weights {
		if w.ExpectedSuccess != 0.5 {
			t.Errorf("%s ExpectedSuccess = %v, want 0.5 (routable)", w.ResourceID, w.ExpectedSuccess)
		}
		if w.Confidence != 0.1 {
			t.Errorf("%s Confidence = %v, want 0.1 (uncertain)", w.ResourceID, w.Confidence)
		}
	}
}
