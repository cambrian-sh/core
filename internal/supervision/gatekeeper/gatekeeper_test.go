package gatekeeper

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

func TestFindCandidates_ProvisionalAgentLast(t *testing.T) {
	active := domain.AgentDefinition{ID: "active", Name: "active", Provisional: false}
	provisional := domain.AgentDefinition{ID: "prov", Name: "prov", Provisional: true}

	registry := newAgentSourceWith(provisional, active)
	gk := NewGatekeeper(registry, defaultGatekeeperCfg())

	candidates, err := gk.FindCandidates(context.Background(), &domain.AuctionTask{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}
	if candidates[0].Agent.ID != "active" {
		t.Errorf("expected first candidate to be active agent, got %q", candidates[0].Agent.ID)
	}
	if candidates[1].Agent.ID != "prov" {
		t.Errorf("expected second candidate to be provisional agent, got %q", candidates[1].Agent.ID)
	}
}

// ADR-0051: a privileged system organ (scout_agent) is kernel-invoked directly and must
// NEVER appear as an auction/EFE candidate for a user task.
func TestFindCandidates_ExcludesSystemAgent(t *testing.T) {
	active := domain.AgentDefinition{ID: "active", Name: "active", Provisional: false}
	scout := domain.AgentDefinition{ID: "scout_agent", Name: "scout", Provisional: false}

	registry := newAgentSourceWith(active, scout)
	gk := NewGatekeeper(registry, defaultGatekeeperCfg())

	candidates, err := gk.FindCandidates(context.Background(), &domain.AuctionTask{ID: "t"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, c := range candidates {
		if c.Agent.ID == "scout_agent" {
			t.Fatalf("scout_agent (system organ) must never be a candidate; got %+v", candidates)
		}
	}
	if len(candidates) != 1 || candidates[0].Agent.ID != "active" {
		t.Errorf("expected only the non-system agent; got %d candidates", len(candidates))
	}
}

func TestFindCandidates_ProvisionalScore(t *testing.T) {
	registry := newAgentSourceWith(domain.AgentDefinition{ID: "prov", Name: "prov", Provisional: true})
	gk := NewGatekeeper(registry, defaultGatekeeperCfg())

	candidates, err := gk.FindCandidates(context.Background(), &domain.AuctionTask{ID: "t2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	if candidates[0].Score != DefaultProvisionalScore {
		t.Errorf("expected Provisional score %.2f, got %.2f", DefaultProvisionalScore, candidates[0].Score)
	}
}

func TestFindCandidates_ActiveAgentScore(t *testing.T) {
	registry := newAgentSourceWith(domain.AgentDefinition{ID: "active", Name: "active", Provisional: false})
	cfg := defaultGatekeeperCfg()
	gk := NewGatekeeper(registry, cfg)

	candidates, err := gk.FindCandidates(context.Background(), &domain.AuctionTask{ID: "t3"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	wantScore := cfg.GatekeeperW1*0.5 + cfg.GatekeeperW2*0.5 + cfg.GatekeeperW3*1.0
	if abs64(candidates[0].Score-wantScore) > 1e-9 {
		t.Errorf("expected neutral score %.4f, got %.4f", wantScore, candidates[0].Score)
	}
}

func TestFindCandidates_NoCompatibleAgents_ReturnsEmpty(t *testing.T) {
	registry := newAgentSourceWith(domain.AgentDefinition{ID: "sql-agent", Name: "sql-agent", Provisional: false})
	registry.manifests["sql-agent"] = &domain.AgentManifest{Tools: []string{"sql"}}
	gk := NewGatekeeper(registry, defaultGatekeeperCfg())

	candidates, err := gk.FindCandidates(context.Background(), &domain.AuctionTask{
		ID:              "t4",
		RequiredFormats: []string{"json"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != 0 {
		t.Errorf("expected 0 candidates, got %d", len(candidates))
	}
}
