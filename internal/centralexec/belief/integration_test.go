package belief

import (
	"context"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// The store satisfies domain.PrecisionProvider: it resolves a candidate set to
// per-resource precision weights for an intent — the seam the InferenceSelector
// consumes (0037-01 ↔ 0037-03).
func TestStore_PrecisionFor_ImplementsProvider(t *testing.T) {
	var _ domain.PrecisionProvider = New(testRegions(), testConfig())

	s := New(testRegions(), testConfig())
	s.SeedPrior("a")
	s.SeedPrior("b")
	for i := 0; i < 5; i++ {
		s.Update("a", "comparison", Outcome{Success: 1.0})
	}

	intent := domain.Intent{ID: "i1", Embedding: []float32{1, 0}} // comparison
	candidates := []domain.AgentDefinition{{ID: "a"}, {ID: "b"}}

	weights, err := s.PrecisionFor(context.Background(), intent, candidates)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(weights) != 2 {
		t.Fatalf("len(weights) = %d, want 2", len(weights))
	}
	byID := map[string]domain.PrecisionWeight{}
	for _, w := range weights {
		byID[w.ResourceID] = w
	}
	if byID["a"].ExpectedSuccess <= byID["b"].ExpectedSuccess {
		t.Errorf("proven resource a (%v) should outweigh unproven b (%v)", byID["a"].ExpectedSuccess, byID["b"].ExpectedSuccess)
	}
}

// The store satisfies domain.RegionSource: Regions reports aggregate belief mass
// so the CapabilityCatalog can project the credible vocabulary (0037-02 ↔ 0037-03).
func TestStore_Regions_ReportsBeliefMass(t *testing.T) {
	var _ domain.RegionSource = New(testRegions(), testConfig())

	s := New(testRegions(), testConfig())
	s.SeedPrior("a")
	for i := 0; i < 5; i++ {
		s.Update("a", "comparison", Outcome{Success: 1.0})
	}
	s.Consolidate()

	regions, err := s.Regions(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	byLabel := map[string]domain.CapabilityRegion{}
	for _, r := range regions {
		byLabel[r.Label] = r
	}
	if byLabel["comparison"].BeliefMass <= 0 {
		t.Errorf("comparison BeliefMass = %v, want > 0 (a resource is credible there)", byLabel["comparison"].BeliefMass)
	}
	if byLabel["summarization"].BeliefMass != 0 {
		t.Errorf("summarization BeliefMass = %v, want 0 (no resource has earned it)", byLabel["summarization"].BeliefMass)
	}
}

// The sub-goal selection prior inherits the slow store (global trust) but
// queries the fast store fresh (ADR-0037 D14): a parent task's recent in-flight
// bias must not leak into an unrelated sub-task binding.
func TestStore_BeliefForSubgoal_InheritsSlowResetsFast(t *testing.T) {
	s := New(testRegions(), testConfig())
	s.SeedPrior("agent")

	// Earn established (consolidated) trust.
	for i := 0; i < 10; i++ {
		s.Update("agent", "comparison", Outcome{Success: 1.0})
	}
	s.Consolidate()

	// A burst of recent (unconsolidated) failures from the current parent task.
	for i := 0; i < 3; i++ {
		s.Update("agent", "comparison", Outcome{Success: 0.0})
	}

	emb := []float32{1, 0}
	combined := s.Belief("agent", emb).ExpectedSuccess           // includes the fast bias
	subgoal := s.BeliefForSubgoal("agent", emb).ExpectedSuccess  // slow store only

	if subgoal <= combined {
		t.Errorf("BeliefForSubgoal=%v should exceed fast-contaminated Belief=%v (inherits slow trust)", subgoal, combined)
	}
	if subgoal <= 0.8 {
		t.Errorf("BeliefForSubgoal=%v should reflect the high consolidated trust", subgoal)
	}
}
