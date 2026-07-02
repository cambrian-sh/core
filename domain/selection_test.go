package domain

import "testing"

// Tracer bullet (ADR-0037 D9, issue 0037-01): the pure EFE minimizer picks a
// single candidate and reports the EFE mechanism. This proves the
// retrieve→precision-weight→EFE-pick path exists end-to-end before any learning.
func TestMinimizeEFE_SingleCandidate(t *testing.T) {
	weights := []PrecisionWeight{
		{ResourceID: "agent-a", ExpectedSuccess: 0.7, Confidence: 0.5},
	}

	sel, err := MinimizeEFE(weights, 0.1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sel.ResourceID != "agent-a" {
		t.Errorf("ResourceID = %q, want agent-a", sel.ResourceID)
	}
	if sel.Mechanism != MechanismEFE {
		t.Errorf("Mechanism = %q, want %q", sel.Mechanism, MechanismEFE)
	}
}

// The EFE pick balances pragmatic value (expected success) against epistemic
// value (exploration of under-sampled resources). These cases are the
// table-driven spec the issue requires (0037-01 acceptance #1).
func TestMinimizeEFE_PragmaticVsEpistemic(t *testing.T) {
	tests := []struct {
		name             string
		weights          []PrecisionWeight
		explorationBonus float64
		wantResource     string
	}{
		{
			name: "pragmatic dominates when confidence is equal",
			weights: []PrecisionWeight{
				{ResourceID: "low", ExpectedSuccess: 0.4, Confidence: 0.5},
				{ResourceID: "high", ExpectedSuccess: 0.8, Confidence: 0.5},
			},
			explorationBonus: 0.1,
			wantResource:     "high",
		},
		{
			name: "epistemic preference: equal success, under-sampled (low confidence) wins",
			weights: []PrecisionWeight{
				{ResourceID: "known", ExpectedSuccess: 0.6, Confidence: 0.9},
				{ResourceID: "novel", ExpectedSuccess: 0.6, Confidence: 0.1},
			},
			explorationBonus: 0.5,
			wantResource:     "novel",
		},
		{
			name: "zero exploration bonus collapses to pure pragmatic",
			weights: []PrecisionWeight{
				{ResourceID: "known", ExpectedSuccess: 0.6, Confidence: 0.9},
				{ResourceID: "novel", ExpectedSuccess: 0.55, Confidence: 0.05},
			},
			explorationBonus: 0.0,
			wantResource:     "known",
		},
		{
			name: "deterministic lexical tie-break on identical EFE",
			weights: []PrecisionWeight{
				{ResourceID: "zebra", ExpectedSuccess: 0.5, Confidence: 0.5},
				{ResourceID: "alpha", ExpectedSuccess: 0.5, Confidence: 0.5},
			},
			explorationBonus: 0.2,
			wantResource:     "alpha",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sel, err := MinimizeEFE(tt.weights, tt.explorationBonus)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if sel.ResourceID != tt.wantResource {
				t.Errorf("ResourceID = %q, want %q", sel.ResourceID, tt.wantResource)
			}
		})
	}
}

func TestMinimizeEFE_EmptyCandidatesIsError(t *testing.T) {
	_, err := MinimizeEFE(nil, 0.1)
	if err != ErrNoCandidates {
		t.Errorf("err = %v, want ErrNoCandidates", err)
	}
}
