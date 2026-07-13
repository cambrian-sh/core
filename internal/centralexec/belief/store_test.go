package belief

import (
	"testing"

	"github.com/cambrian-sh/core/domain"
)

func testRegions() []domain.CapabilityRegion {
	return []domain.CapabilityRegion{
		{Label: "comparison", Centroid: []float32{1, 0}},
		{Label: "summarization", Centroid: []float32{0, 1}},
	}
}

func testConfig() Config {
	return Config{
		PriorExpectedSuccess: 0.5,
		FastAlpha:            0.5,
		SlowAlpha:            0.1,
		ConfidenceK:          5,
		MinSimilarity:        0.5,
	}
}

// Tracer (ADR-0037 D2, 0037-03 #1): a fresh resource seeded only from a verified
// declaration is immediately routable — Belief returns the prior expected
// success at LOW confidence (verified but unproven).
func TestStore_Prior_RoutableAtLowConfidence(t *testing.T) {
	s := New(testRegions(), testConfig())
	s.SeedPrior("fresh-agent")

	b := s.Belief("fresh-agent", []float32{1, 0}) // near the comparison region

	if b.ResourceID != "fresh-agent" {
		t.Errorf("ResourceID = %q, want fresh-agent", b.ResourceID)
	}
	if b.ExpectedSuccess != 0.5 {
		t.Errorf("ExpectedSuccess = %v, want 0.5 (prior, routable)", b.ExpectedSuccess)
	}
	if b.Confidence <= 0 || b.Confidence >= 0.5 {
		t.Errorf("Confidence = %v, want low (0,0.5) — verified but unproven", b.Confidence)
	}
}

// Update is region-resolved (0037-03 #2): outcomes in one region move belief
// only there. "Good at comparison, bad at summarization" must be representable.
func TestStore_Update_IsPerRegionNotGlobal(t *testing.T) {
	s := New(testRegions(), testConfig())
	s.SeedPrior("agent")

	// Several successes in the comparison region.
	for i := 0; i < 5; i++ {
		s.Update("agent", "comparison", Outcome{Success: 1.0})
	}

	comp := s.Belief("agent", []float32{1, 0})   // comparison
	summ := s.Belief("agent", []float32{0, 1})    // summarization (untouched)

	if comp.ExpectedSuccess <= 0.5 {
		t.Errorf("comparison ExpectedSuccess = %v, want raised above prior 0.5", comp.ExpectedSuccess)
	}
	if comp.Confidence <= summ.Confidence {
		t.Errorf("comparison confidence (%v) should exceed untouched summarization (%v)", comp.Confidence, summ.Confidence)
	}
	if summ.ExpectedSuccess != 0.5 {
		t.Errorf("summarization ExpectedSuccess = %v, want still 0.5 (prior — not moved globally)", summ.ExpectedSuccess)
	}
}

// Two resources with identical priors in the same region diverge purely by
// outcome (0037-03 #3) — the test that proves capability is learned, not
// inferred from description similarity.
func TestStore_TopicallySimilarResourcesDivergeByOutcome(t *testing.T) {
	s := New(testRegions(), testConfig())
	s.SeedPrior("winner")
	s.SeedPrior("loser")

	for i := 0; i < 5; i++ {
		s.Update("winner", "comparison", Outcome{Success: 1.0})
		s.Update("loser", "comparison", Outcome{Success: 0.0})
	}

	w := s.Belief("winner", []float32{1, 0})
	l := s.Belief("loser", []float32{1, 0})

	if !(w.ExpectedSuccess > l.ExpectedSuccess) {
		t.Errorf("expected divergence: winner=%v should exceed loser=%v", w.ExpectedSuccess, l.ExpectedSuccess)
	}
	if w.ExpectedSuccess <= 0.5 || l.ExpectedSuccess >= 0.5 {
		t.Errorf("winner should rise above and loser fall below the 0.5 prior: w=%v l=%v", w.ExpectedSuccess, l.ExpectedSuccess)
	}
}

// Offline interleaved consolidation resists a few bad runs (0037-03 #4): once a
// resource has earned high consolidated (slow-store) belief, a short burst of
// failures cannot catastrophically overwrite it — the stability half of CLS.
func TestStore_Consolidation_ResistsFewBadRuns(t *testing.T) {
	s := New(testRegions(), testConfig())
	s.SeedPrior("veteran")

	// Earn established trust, then consolidate into the slow store.
	for i := 0; i < 10; i++ {
		s.Update("veteran", "comparison", Outcome{Success: 1.0})
	}
	s.Consolidate()

	established := s.Belief("veteran", []float32{1, 0}).ExpectedSuccess
	if established <= 0.8 {
		t.Fatalf("post-consolidation belief = %v, want high (>0.8)", established)
	}

	// A few bad runs, then consolidate again.
	for i := 0; i < 3; i++ {
		s.Update("veteran", "comparison", Outcome{Success: 0.0})
	}
	s.Consolidate()

	after := s.Belief("veteran", []float32{1, 0}).ExpectedSuccess
	if after <= 0.7 {
		t.Errorf("belief after a few bad runs = %v, want still high (>0.7) — not catastrophically overwritten", after)
	}
}

// A resource landing in an established cluster inherits a cluster-level schema
// prior (0037-03 #5, the CLS schema fast-path) — warmer than the bare
// verified-declaration prior, but still at low confidence so EFE still explores.
func TestStore_ClusterSchemaFastPath(t *testing.T) {
	regions := []domain.CapabilityRegion{
		{Label: "comparison", Cluster: "analytical", Centroid: []float32{1, 0}},
		{Label: "ranking", Cluster: "analytical", Centroid: []float32{0.7, 0.3}},
		{Label: "summarization", Cluster: "linguistic", Centroid: []float32{0, 1}},
	}
	s := New(regions, testConfig())

	// An expert earns established belief in the analytical cluster.
	s.SeedPrior("expert")
	for i := 0; i < 10; i++ {
		s.Update("expert", "comparison", Outcome{Success: 1.0})
	}
	s.Consolidate()

	// A brand-new resource queries a sibling region in the same cluster.
	s.SeedPrior("newbie")
	b := s.Belief("newbie", []float32{0.7, 0.3}) // ranking ∈ analytical

	if b.ExpectedSuccess <= 0.5 {
		t.Errorf("newbie ranking ExpectedSuccess = %v, want > bare prior 0.5 (cluster schema warm start)", b.ExpectedSuccess)
	}
	if b.Confidence >= 0.5 {
		t.Errorf("newbie Confidence = %v, want still low (schema prior is unproven for this resource)", b.Confidence)
	}

	// A resource in an unrelated cluster gets no schema lift — bare prior.
	lin := s.Belief("newbie", []float32{0, 1})
	if lin.ExpectedSuccess != 0.5 {
		t.Errorf("unrelated cluster ExpectedSuccess = %v, want bare prior 0.5", lin.ExpectedSuccess)
	}
}
