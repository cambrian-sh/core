package centralexec

import (
	"context"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// fakePrecision is a test seam standing in for the CapabilityBelief store
// (0037-03) and the Gatekeeper precision oracle (0037-04). It returns canned
// weights so the InferenceSelector can be tested in isolation.
type fakePrecision struct {
	weights []domain.PrecisionWeight
	gotIntent     domain.Intent
	gotCandidates []domain.AgentDefinition
}

func (f *fakePrecision) PrecisionFor(_ context.Context, intent domain.Intent, candidates []domain.AgentDefinition) ([]domain.PrecisionWeight, error) {
	f.gotIntent = intent
	f.gotCandidates = candidates
	return f.weights, nil
}

// The InferenceSelector binds the EFE-winning resource via the precision oracle
// and the pure MinimizeEFE core — with no Auctioneer on the path (0037-01 #2).
func TestInferenceSelector_BindsEFEWinner(t *testing.T) {
	prec := &fakePrecision{weights: []domain.PrecisionWeight{
		{ResourceID: "weak", ExpectedSuccess: 0.3, Confidence: 0.8},
		{ResourceID: "strong", ExpectedSuccess: 0.9, Confidence: 0.8},
	}}
	sel := &InferenceSelector{Precision: prec, ExplorationBonus: 0.1}

	intent := domain.Intent{ID: "i1", Description: "summarize the report"}
	candidates := []domain.AgentDefinition{{ID: "weak"}, {ID: "strong"}}

	got, err := sel.Select(context.Background(), intent, candidates)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ResourceID != "strong" {
		t.Errorf("ResourceID = %q, want strong", got.ResourceID)
	}
	if got.Mechanism != domain.MechanismEFE {
		t.Errorf("Mechanism = %q, want efe", got.Mechanism)
	}
	// The selector must pass the intent + candidates through to the oracle — it
	// holds no authoritative resource state of its own (0037-01 #5).
	if prec.gotIntent.ID != "i1" || len(prec.gotCandidates) != 2 {
		t.Errorf("oracle not queried with intent+candidates: intent=%q n=%d", prec.gotIntent.ID, len(prec.gotCandidates))
	}
}
