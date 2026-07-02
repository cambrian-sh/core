package centralexec

import (
	"context"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// The Gatekeeper-backed EFE selector discovers candidates through the Gatekeeper
// (reusing the existing declaration/scope filtering) then binds via the EFE pick
// — the self-contained "efe" arm wired into the live dispatch (Fix 4 / 0037-11).
// The cold-start prior is seeded from the Gatekeeper score, so the higher-merit
// candidate wins even though it sorts LATER lexically — the regression guard
// against "every task goes to the alphabetically-first agent".
func TestGatekeeperEFESelector_DiscoversThenPicks(t *testing.T) {
	gk := fakeGatekeeper{candidates: []domain.ScoredCandidate{
		{Agent: domain.AgentDefinition{ID: "alpha"}, Score: 0.4},
		{Agent: domain.AgentDefinition{ID: "beta"}, Score: 0.9},
	}}
	sel := NewGatekeeperEFESelector(gk, 0.1)

	got, err := sel.Select(context.Background(), domain.Intent{ID: "t1", Description: "do a thing"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// beta has the higher Gatekeeper score (0.9 > 0.4) → higher EFE value → bound,
	// despite "alpha" being lexically first. No uniform-seed lexical degeneracy.
	if got.ResourceID != "beta" {
		t.Errorf("ResourceID = %q, want beta (highest Gatekeeper-seeded EFE prior)", got.ResourceID)
	}
	if got.Mechanism != domain.MechanismEFE {
		t.Errorf("Mechanism = %q, want efe", got.Mechanism)
	}
}

// Regression guard for the observed bug: with EQUAL scores the lexical tie-break
// still applies (deterministic), but as soon as scores differ the merit wins —
// proving routing is driven by the Gatekeeper signal, not alphabetical order.
func TestGatekeeperEFESelector_ScoreBeatsLexicalOrder(t *testing.T) {
	gk := fakeGatekeeper{candidates: []domain.ScoredCandidate{
		// "analyst" sorts first alphabetically but is the weakest candidate.
		{Agent: domain.AgentDefinition{ID: "analyst"}, Score: 0.1},
		{Agent: domain.AgentDefinition{ID: "terminal"}, Score: 0.8},
	}}
	sel := NewGatekeeperEFESelector(gk, 0.1)
	got, err := sel.Select(context.Background(), domain.Intent{ID: "t2", Description: "run a command"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ResourceID != "terminal" {
		t.Errorf("ResourceID = %q, want terminal (higher score must beat lexically-first analyst)", got.ResourceID)
	}
}

func TestGatekeeperEFESelector_NoCandidates(t *testing.T) {
	sel := NewGatekeeperEFESelector(fakeGatekeeper{}, 0.1)
	if _, err := sel.Select(context.Background(), domain.Intent{}, nil); err != domain.ErrNoCandidates {
		t.Errorf("err = %v, want ErrNoCandidates", err)
	}
}

var _ domain.ResourceSelector = (*GatekeeperEFESelector)(nil)
