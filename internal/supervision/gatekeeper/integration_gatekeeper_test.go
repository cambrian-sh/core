package gatekeeper

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

func TestIntegration_CorrectCandidateSelection(t *testing.T) {
	reg := newMockAgentDeclarationSource([]domain.AgentDefinition{
		{ID: "sql-high", SourceHash: "h1", Provisional: false},
		{ID: "sql-low", SourceHash: "l1", Provisional: false},
		{ID: "image", SourceHash: "i1", Provisional: false},
		{ID: "sql-prov", SourceHash: "p1", Provisional: true},
	}, map[string]*domain.AgentManifest{
		"sql-high": {Tools: []string{"sql"}},
		"sql-low":  {Tools: []string{"sql"}},
		"image":    {Tools: []string{"image"}},
		"sql-prov": {Tools: []string{"sql"}},
	})

	profiles := map[string]*domain.AgentProfile{
		"sql-high:h1": {TrustScore: 0.9, SuccessRate: 0.9},
		"sql-low:l1":  {TrustScore: 0.6, SuccessRate: 0.6},
	}
	pr := &mockGatekeeperProfileReader{profiles: profiles}

	gk := NewGatekeeper(reg, defaultGatekeeperCfg(), WithProfiles(pr))
	candidates, err := gk.FindCandidates(context.Background(), &domain.AuctionTask{ID: "t-s1"})
	if err != nil {
		t.Fatalf("FindCandidates error: %v", err)
	}

	if len(candidates) != 4 {
		t.Fatalf("expected 4 candidates, got %d: %v", len(candidates), candidateIDs(candidates))
	}
	if candidates[0].Agent.ID != "sql-high" {
		t.Errorf("expected sql-high first, got %q", candidates[0].Agent.ID)
	}
	if candidates[1].Agent.ID != "sql-low" {
		t.Errorf("expected sql-low second, got %q", candidates[1].Agent.ID)
	}
	if candidates[2].Agent.ID != "image" {
		t.Errorf("expected image third, got %q", candidates[2].Agent.ID)
	}
	if candidates[3].Agent.ID != "sql-prov" {
		t.Errorf("expected sql-prov last, got %q", candidates[3].Agent.ID)
	}
	if candidates[0].Score <= candidates[1].Score {
		t.Errorf("sql-high score (%.4f) must exceed sql-low score (%.4f)", candidates[0].Score, candidates[1].Score)
	}
	if candidates[2].Score <= candidates[3].Score {
		t.Errorf("image score (%.4f) must exceed sql-prov score (%.4f)", candidates[2].Score, candidates[3].Score)
	}
	if candidates[3].Score != DefaultProvisionalScore {
		t.Errorf("sql-prov must have DefaultProvisionalScore %.4f, got %.4f", DefaultProvisionalScore, candidates[3].Score)
	}
}

func TestIntegration_AllProvisional(t *testing.T) {
	reg := newMockAgentDeclarationSource([]domain.AgentDefinition{
		{ID: "a1", Provisional: true},
		{ID: "a2", Provisional: true},
		{ID: "a3", Provisional: true},
	}, map[string]*domain.AgentManifest{
		"a1": {Tools: []string{"sql"}},
		"a2": {Tools: []string{"sql"}},
		"a3": {Tools: []string{"sql"}},
	})

	gk := NewGatekeeper(reg, defaultGatekeeperCfg())
	candidates, err := gk.FindCandidates(context.Background(), &domain.AuctionTask{ID: "t-s2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != 3 {
		t.Fatalf("expected 3 candidates, got %d", len(candidates))
	}
	for _, c := range candidates {
		if c.Score != DefaultProvisionalScore {
			t.Errorf("agent %q: expected DefaultProvisionalScore %.4f, got %.4f",
				c.Agent.ID, DefaultProvisionalScore, c.Score)
		}
	}
}
