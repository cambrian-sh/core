package gatekeeper

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

func TestGatekeeperScore_HighProfileScoresHigher(t *testing.T) {
	high := domain.AgentDefinition{ID: "high", Name: "high", Provisional: false}
	low := domain.AgentDefinition{ID: "low", Name: "low", Provisional: false}

	profiles := &mockGatekeeperProfileReader{
		profiles: map[string]*domain.AgentProfile{
			"high:": {SuccessRate: 1.0, TrustScore: 1.0, NetworkLatencyMedianMs: 10, ComputationLatencyMedianMs: 10},
			"low:":  {SuccessRate: 0.5, TrustScore: 0.5, NetworkLatencyMedianMs: 100, ComputationLatencyMedianMs: 100},
		},
	}

	registry := newMockAgentDeclarationSource([]domain.AgentDefinition{high, low}, nil)
	gk := NewGatekeeper(registry, defaultTestExecCfg(), WithProfiles(profiles))

	candidates, err := gk.FindCandidates(context.Background(), &domain.AuctionTask{ID: "t11"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) < 2 {
		t.Fatalf("expected at least 2 candidates, got %d", len(candidates))
	}
	if candidates[0].Agent.ID != "high" {
		t.Errorf("expected 'high' agent first, got %q", candidates[0].Agent.ID)
	}
	if candidates[0].Score <= candidates[1].Score {
		t.Errorf("high score %.4f should exceed low score %.4f", candidates[0].Score, candidates[1].Score)
	}
}

func TestGatekeeperScore_NilProfile_NeutralScore(t *testing.T) {
	agent := domain.AgentDefinition{ID: "noProfile", Name: "noProfile", Provisional: false}
	profiles := &mockGatekeeperProfileReader{profiles: map[string]*domain.AgentProfile{}}

	registry := newMockAgentDeclarationSource([]domain.AgentDefinition{agent}, nil)
	cfg := defaultTestExecCfg()
	gk := NewGatekeeper(registry, cfg, WithProfiles(profiles))

	candidates, err := gk.FindCandidates(context.Background(), &domain.AuctionTask{ID: "t12"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	want := 0.4*0.5 + 0.4*0.5 + 0.2*1.0
	if abs64(candidates[0].Score-want) > 1e-9 {
		t.Errorf("neutral score = %.6f, want %.6f", candidates[0].Score, want)
	}
}

func TestGatekeeperScore_TopNCandidates(t *testing.T) {
	var agentList []domain.AgentDefinition
	profileMap := map[string]*domain.AgentProfile{}
	for i := 0; i < 7; i++ {
		id := string(rune('a'+i)) + "-agent"
		agentList = append(agentList, domain.AgentDefinition{ID: id, Name: id, Provisional: false})
		profileMap[id+":"] = &domain.AgentProfile{
			SuccessRate: float64(i) * 0.1,
			TrustScore:  float64(i) * 0.1,
		}
	}
	profiles := &mockGatekeeperProfileReader{profiles: profileMap}
	registry := newMockAgentDeclarationSource(agentList, nil)
	cfg := defaultTestExecCfg()
	cfg.GatekeeperMaxCandidates = 5
	gk := NewGatekeeper(registry, cfg, WithProfiles(profiles))

	candidates, err := gk.FindCandidates(context.Background(), &domain.AuctionTask{ID: "t13"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) > 5 {
		t.Errorf("expected at most 5 candidates, got %d", len(candidates))
	}
}

func TestGatekeeperScore_ProfileProvisional_PenaltyApplied(t *testing.T) {
	active := domain.AgentDefinition{ID: "active", Name: "active", Provisional: false}
	postInterview := domain.AgentDefinition{ID: "postinterview", Name: "postinterview", Provisional: false}

	profiles := &mockGatekeeperProfileReader{
		profiles: map[string]*domain.AgentProfile{
			"active:":        {SuccessRate: 0.8, TrustScore: 0.8, Provisional: false},
			"postinterview:": {SuccessRate: 0.8, TrustScore: 0.8, Provisional: true},
		},
	}
	registry := newMockAgentDeclarationSource([]domain.AgentDefinition{active, postInterview}, nil)
	cfg := defaultTestExecCfg()
	cfg.ColdStartPenaltyMultiplier = 0.6
	gk := NewGatekeeper(registry, cfg, WithProfiles(profiles))

	candidates, err := gk.FindCandidates(context.Background(), &domain.AuctionTask{ID: "t18"})
	if err != nil {
		t.Fatalf("FindCandidates: %v", err)
	}

	scores := map[string]float64{}
	for _, c := range candidates {
		scores[c.Agent.ID] = c.Score
	}
	if scores["postinterview"] >= scores["active"] {
		t.Errorf("post-Interview Provisional score (%.4f) should be lower than active (%.4f)",
			scores["postinterview"], scores["active"])
	}
}

func TestGatekeeperScore_ProvisionalLast(t *testing.T) {
	active := domain.AgentDefinition{ID: "active", Name: "active", Provisional: false}
	prov := domain.AgentDefinition{ID: "prov", Name: "prov", Provisional: true}

	profiles := &mockGatekeeperProfileReader{
		profiles: map[string]*domain.AgentProfile{
			"active:": {SuccessRate: 0.5, TrustScore: 0.5},
		},
	}
	registry := newMockAgentDeclarationSource([]domain.AgentDefinition{prov, active}, nil)
	gk := NewGatekeeper(registry, defaultTestExecCfg(), WithProfiles(profiles))

	candidates, err := gk.FindCandidates(context.Background(), &domain.AuctionTask{ID: "t14"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}
	last := candidates[len(candidates)-1]
	if !last.Agent.Provisional {
		t.Errorf("last candidate should be provisional, got %q (provisional=%v)", last.Agent.ID, last.Agent.Provisional)
	}
	if last.Score >= candidates[0].Score {
		t.Errorf("provisional score %.4f should be less than active score %.4f", last.Score, candidates[0].Score)
	}
}
