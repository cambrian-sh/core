package gatekeeper

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// Since the interview deletion (2026-11-05) a provisional agent is not ranked last
// by construction: with no evidence on either side it ties the active agent on the
// neutral prior, and both stay candidates.
func TestFindCandidates_ProvisionalAgentTiesOnNeutralPrior(t *testing.T) {
	active := domain.AgentDefinition{ID: "active", Name: "active", Provisional: false}
	provisional := domain.AgentDefinition{ID: "prov", Name: "prov", Provisional: true}

	registry := newAgentSourceWith(provisional, active)
	gk := NewGatekeeper(registry, defaultGatekeeperCfg())

	candidates, err := gk.FindCandidates(context.Background(), &domain.DispatchTask{ID: "t1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}
	if candidates[0].Score != candidates[1].Score {
		t.Errorf("scores %.4f (%s) vs %.4f (%s), want a neutral-prior tie",
			candidates[0].Score, candidates[0].Agent.ID, candidates[1].Score, candidates[1].Agent.ID)
	}
}

// A privileged system organ (retrieval_agent) is kernel-invoked directly and must
// NEVER appear as an auction/EFE candidate for a user task.
func TestFindCandidates_ExcludesSystemAgent(t *testing.T) {
	active := domain.AgentDefinition{ID: "active", Name: "active", Provisional: false}
	sysAgent := domain.AgentDefinition{ID: "retrieval_agent", Name: "retrieval", Provisional: false}

	registry := newAgentSourceWith(active, sysAgent)
	gk := NewGatekeeper(registry, defaultGatekeeperCfg())

	candidates, err := gk.FindCandidates(context.Background(), &domain.DispatchTask{ID: "t"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, c := range candidates {
		if c.Agent.ID == "retrieval_agent" {
			t.Fatalf("retrieval_agent (system organ) must never be a candidate; got %+v", candidates)
		}
	}
	if len(candidates) != 1 || candidates[0].Agent.ID != "active" {
		t.Errorf("expected only the non-system agent; got %d candidates", len(candidates))
	}
}

func TestFindCandidates_ProvisionalScore(t *testing.T) {
	registry := newAgentSourceWith(domain.AgentDefinition{ID: "prov", Name: "prov", Provisional: true})
	gk := NewGatekeeper(registry, defaultGatekeeperCfg())

	candidates, err := gk.FindCandidates(context.Background(), &domain.DispatchTask{ID: "t2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	// A provisional agent with no profile is scored on the neutral prior (the flat
	// DefaultProvisionalScore was retired with the interview, 2026-11-05).
	if want := ScoreMerit(nil, domain.TraitCognitive, nil, defaultGatekeeperCfg(), nil).Score; candidates[0].Score != want {
		t.Errorf("expected the neutral-prior score %.4f, got %.4f", want, candidates[0].Score)
	}
}

func TestFindCandidates_ActiveAgentScore(t *testing.T) {
	registry := newAgentSourceWith(domain.AgentDefinition{ID: "active", Name: "active", Provisional: false})
	cfg := defaultGatekeeperCfg()
	gk := NewGatekeeper(registry, cfg)

	candidates, err := gk.FindCandidates(context.Background(), &domain.DispatchTask{ID: "t3"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	// ADR-0139 D6: no latency data in the slate -> latency term 0, not the full w3.
	wantScore := cfg.Gatekeeper.GatekeeperW1*0.5 + cfg.Gatekeeper.GatekeeperW2*0.5
	if abs64(candidates[0].Score-wantScore) > 1e-9 {
		t.Errorf("expected neutral score %.4f, got %.4f", wantScore, candidates[0].Score)
	}
}

func TestFindCandidates_NoCompatibleAgents_ReturnsEmpty(t *testing.T) {
	registry := newAgentSourceWith(domain.AgentDefinition{ID: "sql-agent", Name: "sql-agent", Provisional: false})
	registry.manifests["sql-agent"] = &domain.AgentManifest{Tools: []string{"sql"}}
	gk := NewGatekeeper(registry, defaultGatekeeperCfg())

	candidates, err := gk.FindCandidates(context.Background(), &domain.DispatchTask{
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
