package centralexec

import (
	"context"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// recordingSolicitor records which candidates were asked for a live proposal
// and returns canned self-assessed confidences keyed by resource ID.
type recordingSolicitor struct {
	bids   map[string]float64
	asked  []string
}

func (r *recordingSolicitor) SolicitBid(_ context.Context, _ domain.Intent, c domain.AgentDefinition) (float64, bool, error) {
	r.asked = append(r.asked, c.ID)
	b, ok := r.bids[c.ID]
	return b, ok, nil
}

// A peaked posterior (a clear EFE winner) must NOT solicit a live bid — the bid
// is pulled only when uncertain (ADR-0037 D6, 0037-05).
func TestInferenceSelector_PeakedPosterior_NoSolicitation(t *testing.T) {
	prec := &fakePrecision{weights: []domain.PrecisionWeight{
		{ResourceID: "clear", ExpectedSuccess: 0.95, Confidence: 0.9},
		{ResourceID: "weak", ExpectedSuccess: 0.2, Confidence: 0.9},
	}}
	sol := &recordingSolicitor{bids: map[string]float64{}}
	sel := &InferenceSelector{Precision: prec, ExplorationBonus: 0.1, Solicitor: sol, FlatMargin: 0.05, SoftBidWeight: 0.5}

	got, err := sel.Select(context.Background(), domain.Intent{ID: "i"}, []domain.AgentDefinition{{ID: "clear"}, {ID: "weak"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sol.asked) != 0 {
		t.Errorf("solicited bids on a peaked posterior: %v", sol.asked)
	}
	if got.SolicitedBid {
		t.Error("SolicitedBid = true, want false on a peaked posterior")
	}
	if got.ResourceID != "clear" {
		t.Errorf("ResourceID = %q, want clear", got.ResourceID)
	}
}

// A flat posterior (near-tie) solicits a live bid; the input-conditioned
// self-assessment folds in as a soft prior and breaks the tie (D6).
func TestInferenceSelector_FlatPosterior_SolicitsAndFolds(t *testing.T) {
	prec := &fakePrecision{weights: []domain.PrecisionWeight{
		{ResourceID: "a", ExpectedSuccess: 0.6, Confidence: 0.5},
		{ResourceID: "b", ExpectedSuccess: 0.6, Confidence: 0.5},
	}}
	// b self-assesses much higher for this specific intent (e.g. "I hold the API key").
	sol := &recordingSolicitor{bids: map[string]float64{"a": 0.4, "b": 0.95}}
	sel := &InferenceSelector{Precision: prec, ExplorationBonus: 0.1, Solicitor: sol, FlatMargin: 0.05, SoftBidWeight: 0.5}

	got, err := sel.Select(context.Background(), domain.Intent{ID: "i"}, []domain.AgentDefinition{{ID: "a"}, {ID: "b"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sol.asked) == 0 {
		t.Error("expected a solicited bid on a flat posterior")
	}
	if !got.SolicitedBid {
		t.Error("SolicitedBid = false, want true on a flat posterior")
	}
	if got.ResourceID != "b" {
		t.Errorf("ResourceID = %q, want b (its live self-assessment broke the tie)", got.ResourceID)
	}
}

// With no solicitor wired, a flat posterior just picks deterministically — the
// bid is optional, never mandatory (no bid round remains).
func TestInferenceSelector_NoSolicitor_FlatPosteriorStillPicks(t *testing.T) {
	prec := &fakePrecision{weights: []domain.PrecisionWeight{
		{ResourceID: "alpha", ExpectedSuccess: 0.6, Confidence: 0.5},
		{ResourceID: "beta", ExpectedSuccess: 0.6, Confidence: 0.5},
	}}
	sel := &InferenceSelector{Precision: prec, ExplorationBonus: 0.1}
	got, err := sel.Select(context.Background(), domain.Intent{}, []domain.AgentDefinition{{ID: "alpha"}, {ID: "beta"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.SolicitedBid {
		t.Error("SolicitedBid = true with no solicitor wired")
	}
	if got.ResourceID != "alpha" {
		t.Errorf("ResourceID = %q, want alpha (lexical tie-break)", got.ResourceID)
	}
}
