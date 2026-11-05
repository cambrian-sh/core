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

	candidates, err := gk.FindCandidates(context.Background(), &domain.DispatchTask{ID: "t11"})
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

	candidates, err := gk.FindCandidates(context.Background(), &domain.DispatchTask{ID: "t12"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	// ADR-0139 D6: no latency data and no peer with any -> neutral latency term 0
	// (it used to be the full w3 = 0.2 bonus).
	want := 0.4*0.5 + 0.4*0.5
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
	cfg.Gatekeeper.GatekeeperMaxCandidates = 5
	gk := NewGatekeeper(registry, cfg, WithProfiles(profiles))

	candidates, err := gk.FindCandidates(context.Background(), &domain.DispatchTask{ID: "t13"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) > 5 {
		t.Errorf("expected at most 5 candidates, got %d", len(candidates))
	}
}

// The ×0.6 cold-start penalty on a provisional PROFILE was retired 2026-11-05 with
// the LLM interview (its only writer). A legacy row still flagged provisional
// scores exactly like the same evidence without the flag.
func TestGatekeeperScore_ProfileProvisional_NoLongerPenalised(t *testing.T) {
	active := domain.AgentDefinition{ID: "active", Name: "active", Provisional: false}
	legacy := domain.AgentDefinition{ID: "legacy", Name: "legacy", Provisional: false}

	profiles := &mockGatekeeperProfileReader{
		profiles: map[string]*domain.AgentProfile{
			"active:": {SuccessRate: 0.8, TrustScore: 0.8, Provisional: false},
			"legacy:": {SuccessRate: 0.8, TrustScore: 0.8, Provisional: true},
		},
	}
	registry := newMockAgentDeclarationSource([]domain.AgentDefinition{active, legacy}, nil)
	gk := NewGatekeeper(registry, defaultTestExecCfg(), WithProfiles(profiles))

	candidates, err := gk.FindCandidates(context.Background(), &domain.DispatchTask{ID: "t18"})
	if err != nil {
		t.Fatalf("FindCandidates: %v", err)
	}
	scores := map[string]float64{}
	for _, c := range candidates {
		scores[c.Agent.ID] = c.Score
	}
	if scores["legacy"] != scores["active"] {
		t.Errorf("provisional-flagged profile scored %.4f, want the unflagged %.4f (no penalty)",
			scores["legacy"], scores["active"])
	}
}

// Hard requirement of the interview deletion (2026-11-05): an agent is routable
// right after boot. A provisional agent — registered, profile not yet seeded —
// is a candidate on the NEUTRAL prior, the same score as a seeded agent that has
// no verified outcomes yet, instead of the old flat 0.1 that waited for an
// interview to promote it.
func TestGatekeeperScore_ProvisionalRoutesOnNeutralPrior(t *testing.T) {
	seeded := domain.AgentDefinition{ID: "seeded", Name: "seeded", Provisional: false}
	prov := domain.AgentDefinition{ID: "prov", Name: "prov", Provisional: true}

	profiles := &mockGatekeeperProfileReader{
		profiles: map[string]*domain.AgentProfile{
			"seeded:": {SuccessRate: 0.5, TrustScore: 0.5},
		},
	}
	registry := newMockAgentDeclarationSource([]domain.AgentDefinition{prov, seeded}, nil)
	cfg := defaultTestExecCfg()
	gk := NewGatekeeper(registry, cfg, WithProfiles(profiles))

	candidates, err := gk.FindCandidates(context.Background(), &domain.DispatchTask{ID: "t14"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}
	want := ScoreMerit(nil, domain.TraitCognitive, nil, cfg, nil).Score
	for _, c := range candidates {
		if c.Score != want {
			t.Errorf("%s scored %.4f, want the neutral prior %.4f", c.Agent.ID, c.Score, want)
		}
	}
}

// ADR-0139 D6 (T-20261104-merit-math-distorts-routing-interview-grade-is): missing
// latency is NEUTRAL. An agent with no latency data takes the mean latency term of
// the slate's measured agents, instead of the full w3 bonus that once let a
// data-less agent outrank an identical, measured one.
func TestGatekeeperScore_MissingLatencyTakesSlateMean(t *testing.T) {
	fast := domain.AgentDefinition{ID: "fast"}
	slow := domain.AgentDefinition{ID: "slow"}
	blank := domain.AgentDefinition{ID: "blank"}
	profiles := &mockGatekeeperProfileReader{profiles: map[string]*domain.AgentProfile{
		"fast:":  {SuccessRate: 0.5, TrustScore: 0.5, NetworkLatencyMedianMs: 50, ComputationLatencyMedianMs: 50},
		"slow:":  {SuccessRate: 0.5, TrustScore: 0.5, NetworkLatencyMedianMs: 500, ComputationLatencyMedianMs: 500},
		"blank:": {SuccessRate: 0.5, TrustScore: 0.5},
	}}
	cfg := defaultTestExecCfg()
	cfg.Plan.ContextGrowthK = 0
	gk := NewGatekeeper(newMockAgentDeclarationSource([]domain.AgentDefinition{fast, slow, blank}, nil), cfg, WithProfiles(profiles))

	cands, err := gk.FindCandidates(context.Background(), &domain.DispatchTask{ID: "lat"})
	if err != nil {
		t.Fatalf("FindCandidates: %v", err)
	}
	score := map[string]float64{}
	for _, c := range cands {
		score[c.Agent.ID] = c.Score
	}
	base := 0.4*0.5 + 0.4*0.5
	fastTerm, slowTerm := 0.2/100.0, 0.2/1000.0
	want := base + (fastTerm+slowTerm)/2
	if abs64(score["blank"]-want) > 1e-12 {
		t.Fatalf("blank score = %.6f, want slate-mean %.6f", score["blank"], want)
	}
	if !(score["fast"] > score["blank"] && score["blank"] > score["slow"]) {
		t.Fatalf("want fast > blank > slow, got %+v", score)
	}
}
