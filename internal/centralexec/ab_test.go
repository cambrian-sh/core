package centralexec

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// fakeGatekeeper returns canned scored candidates for the AuctionSelector arm.
type fakeGatekeeper struct {
	candidates []domain.ScoredCandidate
}

func (f fakeGatekeeper) FindCandidates(_ context.Context, _ *domain.AuctionTask) ([]domain.ScoredCandidate, error) {
	return f.candidates, nil
}
func (f fakeGatekeeper) FindModelCandidates(_ context.Context, _ []string) ([]domain.ScoredCandidate, error) {
	return nil, nil
}

// The AuctionSelector is the A/B control arm: it selects the highest
// Gatekeeper-scored candidate and tags the Selection "auction" so the benchmark
// can partition metrics by mechanism (PRD-0037 A/B coexistence).
func TestAuctionSelector_PicksHighestScoreTaggedAuction(t *testing.T) {
	gk := fakeGatekeeper{candidates: []domain.ScoredCandidate{
		{Agent: domain.AgentDefinition{ID: "a"}, Score: 0.4},
		{Agent: domain.AgentDefinition{ID: "b"}, Score: 0.9},
	}}
	sel := &AuctionSelector{Gatekeeper: gk}

	got, err := sel.Select(context.Background(), domain.Intent{Description: "do a thing"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ResourceID != "b" {
		t.Errorf("ResourceID = %q, want b (highest gatekeeper score)", got.ResourceID)
	}
	if got.Mechanism != domain.MechanismAuction {
		t.Errorf("Mechanism = %q, want auction", got.Mechanism)
	}
}

// Both arms satisfy the one ResourceSelector abstraction (PRD-0037).
var (
	_ domain.ResourceSelector = (*AuctionSelector)(nil)
	_ domain.ResourceSelector = (*InferenceSelector)(nil)
)

// Variant assignment is session-scoped (PRD-0037): every step in a plan uses the
// same mechanism, so causal attribution stays clean. Pinned modes are absolute;
// "auto" splits deterministically by session at the configured traffic percent.
func TestAssignVariant(t *testing.T) {
	if got := AssignVariant("auction", 100, "s1"); got != domain.MechanismAuction {
		t.Errorf(`"auction" mode = %q, want auction even at 100%%`, got)
	}
	if got := AssignVariant("efe", 0, "s1"); got != domain.MechanismEFE {
		t.Errorf(`"efe" mode = %q, want efe even at 0%%`, got)
	}
	if got := AssignVariant("auto", 0, "s1"); got != domain.MechanismAuction {
		t.Errorf(`"auto" at 0%% = %q, want auction (safe rollout)`, got)
	}
	if got := AssignVariant("auto", 100, "s1"); got != domain.MechanismEFE {
		t.Errorf(`"auto" at 100%% = %q, want efe`, got)
	}
	// Same session id is stable across calls (session-scoped, not per-step).
	first := AssignVariant("auto", 50, "stable-session")
	for i := 0; i < 5; i++ {
		if AssignVariant("auto", 50, "stable-session") != first {
			t.Fatal("variant assignment must be stable for a fixed session id")
		}
	}
}
