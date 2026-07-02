package centralexec

import (
	"context"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/internal/centralexec/belief"
	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// blockOne blocks a single resource id (a policy knowledge source).
type blockOne struct{ id string }

func (b blockOne) Evaluate(_ context.Context, _ domain.Intent, c domain.AgentDefinition) PolicyVerdict {
	if c.ID == b.id {
		return PolicyVerdict{Blocked: true}
	}
	return PolicyVerdict{PrecisionMultiplier: 1.0}
}

// End-to-end Phase-1 wiring proof (0037-11): the belief store (0037-03), the
// Gatekeeper precision shaper (0037-04), the EFE InferenceSelector (0037-01) and
// the CapabilityCatalog (0037-02) compose into one selection mechanism — with no
// Auctioneer on the path. A trained, policy-allowed resource is bound; a blocked
// one never is; the catalog grounds the region as reachable.
func TestPhase1_FullStackComposes(t *testing.T) {
	regions := []domain.CapabilityRegion{
		{Label: "analysis", Centroid: []float32{1, 0}},
		{Label: "summarization", Centroid: []float32{0, 1}},
	}
	store := belief.New(regions, belief.Config{
		PriorExpectedSuccess: 0.5, FastAlpha: 0.5, SlowAlpha: 0.1, ConfidenceK: 3, MinSimilarity: 0.5,
	})
	for _, id := range []string{"proven", "blocked", "fresh"} {
		store.SeedPrior(id)
	}
	// "proven" earns analysis; "blocked" would too, but policy blocks it.
	for i := 0; i < 8; i++ {
		store.Update("proven", "analysis", belief.Outcome{Success: 1.0})
		store.Update("blocked", "analysis", belief.Outcome{Success: 1.0})
	}
	store.Consolidate()

	// Compose: belief store as base precision, shaped by the Gatekeeper block.
	shaped := &ShapedPrecisionProvider{
		Base:    store,
		Shapers: []PrecisionShaper{PolicyPrecisionShaper{Policy: blockOne{id: "blocked"}}},
	}
	selector := &InferenceSelector{Precision: shaped, ExplorationBonus: 0.1}

	intent := domain.Intent{ID: "i1", Description: "analyze the report", Embedding: []float32{1, 0}}
	candidates := []domain.AgentDefinition{{ID: "proven"}, {ID: "blocked"}, {ID: "fresh"}}

	sel, err := selector.Select(context.Background(), intent, candidates)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sel.ResourceID != "proven" {
		t.Errorf("bound %q, want proven (trained + policy-allowed)", sel.ResourceID)
	}
	if sel.Mechanism != domain.MechanismEFE {
		t.Errorf("Mechanism = %q, want efe", sel.Mechanism)
	}

	// The catalog grounds analysis as a reachable (credible) capability region.
	catalog := &CapabilityCatalog{Source: store, MinBeliefMass: 0.1, MinSimilarity: 0.5}
	_, reachable, err := catalog.Reachable(context.Background(), []float32{1, 0})
	if err != nil {
		t.Fatalf("catalog error: %v", err)
	}
	if !reachable {
		t.Error("analysis intent should be reachable — proven resource has credible mass there")
	}
}
