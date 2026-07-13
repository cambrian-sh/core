package centralexec

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/internal/centralexec/belief"
	"github.com/cambrian-sh/core/domain"
)

// Model selection minimizes EFE with cost as a first-class term (ADR-0037 D16):
// for a quality-critical intent a strong-but-expensive model beats a
// weak-but-cheap one; the cost only tips otherwise-equal choices.
func TestModelSelector_CostIsFirstClass(t *testing.T) {
	// Both models look equally "confident"; quality differs by region.
	prec := &fakePrecision{weights: []domain.PrecisionWeight{
		{ResourceID: "weak-cheap", ExpectedSuccess: 0.3, Confidence: 0.9},
		{ResourceID: "strong-pricey", ExpectedSuccess: 0.95, Confidence: 0.9},
	}}
	sel := &ModelSelector{
		Quality:          prec,
		Costs:            map[string]float64{"weak-cheap": 0.05, "strong-pricey": 0.5},
		ExplorationBonus: 0.0,
	}
	got, err := sel.Select(context.Background(), domain.Intent{ID: "complex-code"},
		[]domain.AgentDefinition{{ID: "weak-cheap"}, {ID: "strong-pricey"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ResourceID != "strong-pricey" {
		t.Errorf("ResourceID = %q, want strong-pricey (quality dominates cost on a hard intent)", got.ResourceID)
	}
	if got.Mechanism != domain.MechanismEFE {
		t.Errorf("Mechanism = %q, want efe", got.Mechanism)
	}
}

func TestModelSelector_CostTiebreaksEqualQuality(t *testing.T) {
	prec := &fakePrecision{weights: []domain.PrecisionWeight{
		{ResourceID: "cheap", ExpectedSuccess: 0.8, Confidence: 0.9},
		{ResourceID: "pricey", ExpectedSuccess: 0.8, Confidence: 0.9},
	}}
	sel := &ModelSelector{
		Quality: prec,
		Costs:   map[string]float64{"cheap": 0.05, "pricey": 0.4},
	}
	got, err := sel.Select(context.Background(), domain.Intent{}, []domain.AgentDefinition{{ID: "cheap"}, {ID: "pricey"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ResourceID != "cheap" {
		t.Errorf("ResourceID = %q, want cheap (cost breaks an equal-quality tie)", got.ResourceID)
	}
}

// Region-resolved learning over the SAME belief store (D16): a model that has
// learned it is poor at complex-code is not chosen there even though it is cheap,
// but is still chosen in a region where it performs well.
func TestModelSelector_RegionResolvedLearning(t *testing.T) {
	regions := []domain.CapabilityRegion{
		{Label: "complex-code", Centroid: []float32{1, 0}},
		{Label: "summarization", Centroid: []float32{0, 1}},
	}
	store := belief.New(regions, belief.Config{PriorExpectedSuccess: 0.5, FastAlpha: 0.5, SlowAlpha: 0.1, ConfidenceK: 2, MinSimilarity: 0.5})
	store.SeedPrior("qwen-small")
	store.SeedPrior("opus")

	// qwen-small: bad at complex-code, good at summarization. opus: good at both.
	for i := 0; i < 6; i++ {
		store.Update("qwen-small", "complex-code", belief.Outcome{Success: 0.0})
		store.Update("qwen-small", "summarization", belief.Outcome{Success: 1.0})
		store.Update("opus", "complex-code", belief.Outcome{Success: 1.0})
		store.Update("opus", "summarization", belief.Outcome{Success: 1.0})
	}

	sel := &ModelSelector{
		Quality: store,
		Costs:   map[string]float64{"qwen-small": 0.02, "opus": 0.6},
	}
	models := []domain.AgentDefinition{{ID: "qwen-small"}, {ID: "opus"}}

	code, _ := sel.Select(context.Background(), domain.Intent{Embedding: []float32{1, 0}}, models)
	if code.ResourceID != "opus" {
		t.Errorf("complex-code → %q, want opus (learned to stop sending hard code to the weak model)", code.ResourceID)
	}
	summ, _ := sel.Select(context.Background(), domain.Intent{Embedding: []float32{0, 1}}, models)
	if summ.ResourceID != "qwen-small" {
		t.Errorf("summarization → %q, want qwen-small (good enough + far cheaper)", summ.ResourceID)
	}
}
