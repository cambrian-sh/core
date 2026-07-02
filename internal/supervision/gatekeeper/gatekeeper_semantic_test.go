package gatekeeper

import (
	"context"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

func TestFindCandidates_Layer2_EliminatesBelowThreshold(t *testing.T) {
	above := domain.AgentDefinition{ID: "above", Provisional: false}
	below := domain.AgentDefinition{ID: "below", Provisional: false}
	reg := newMockAgentDeclarationSource([]domain.AgentDefinition{above, below}, nil)

	// DefaultSimilarityThreshold is 0.2; 0.1 is below and should be eliminated.
	searcher := &fakeInterviewSearcher{results: map[string]float64{"above": 0.8, "below": 0.1}}
	embedder := &mockEmbedder{}

	gk := NewGatekeeper(reg, defaultGatekeeperCfg(), WithEmbedder(embedder), WithSearcher(searcher))
	candidates, err := gk.FindCandidates(context.Background(), &domain.AuctionTask{ID: "t-l2-1", Description: "find something"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate (above), got %d: %v", len(candidates), candidateIDs(candidates))
	}
	if candidates[0].Agent.ID != "above" {
		t.Errorf("expected 'above', got %q", candidates[0].Agent.ID)
	}
}

func TestFindCandidates_Layer2_FormatIncompatibleEliminated(t *testing.T) {
	ok := domain.AgentDefinition{ID: "ok", Provisional: false}
	bad := domain.AgentDefinition{ID: "bad", Provisional: false}
	reg := newMockAgentDeclarationSource([]domain.AgentDefinition{ok, bad}, map[string]*domain.AgentManifest{
		"ok":  {SupportedFormats: []string{"json"}},
		"bad": {SupportedFormats: []string{"xml"}},
	})

	searcher := &fakeInterviewSearcher{results: map[string]float64{"ok": 0.9, "bad": 0.9}}
	embedder := &mockEmbedder{}

	gk := NewGatekeeper(reg, defaultGatekeeperCfg(), WithEmbedder(embedder), WithSearcher(searcher))
	candidates, err := gk.FindCandidates(context.Background(), &domain.AuctionTask{
		ID: "t-l2-2", Description: "json task", RequiredFormats: []string{"json"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected 1, got %d: %v", len(candidates), candidateIDs(candidates))
	}
	if candidates[0].Agent.ID != "ok" {
		t.Errorf("expected 'ok', got %q", candidates[0].Agent.ID)
	}
}

func TestFindCandidates_Layer2_MeritOrderPreserved(t *testing.T) {
	high := domain.AgentDefinition{ID: "high", SourceHash: "h1", Provisional: false}
	low := domain.AgentDefinition{ID: "low", SourceHash: "l1", Provisional: false}
	reg := newMockAgentDeclarationSource([]domain.AgentDefinition{high, low}, nil)

	profiles := &mockGatekeeperProfileReader{profiles: map[string]*domain.AgentProfile{
		"high:h1": {TrustScore: 0.9, SuccessRate: 0.9},
		"low:l1":  {TrustScore: 0.3, SuccessRate: 0.3},
	}}

	searcher := &fakeInterviewSearcher{results: map[string]float64{"high": 0.9, "low": 0.7}}
	embedder := &mockEmbedder{}

	gk := NewGatekeeper(reg, defaultGatekeeperCfg(), WithProfiles(profiles), WithEmbedder(embedder), WithSearcher(searcher))
	candidates, err := gk.FindCandidates(context.Background(), &domain.AuctionTask{ID: "t-l2-3", Description: "query task"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("expected 2, got %d: %v", len(candidates), candidateIDs(candidates))
	}
	if candidates[0].Agent.ID != "high" {
		t.Errorf("expected 'high' first, got %q", candidates[0].Agent.ID)
	}
	if candidates[0].Score <= candidates[1].Score {
		t.Errorf("high score (%.4f) must exceed low score (%.4f)", candidates[0].Score, candidates[1].Score)
	}
}

func TestFindCandidates_Layer2_ProvisionalBypassesGate(t *testing.T) {
	active := domain.AgentDefinition{ID: "active", Provisional: false}
	prov := domain.AgentDefinition{ID: "prov", Provisional: true}
	reg := newMockAgentDeclarationSource([]domain.AgentDefinition{active, prov}, nil)

	// DefaultSimilarityThreshold is 0.2; 0.1 is below and should eliminate the active agent.
	searcher := &fakeInterviewSearcher{results: map[string]float64{"active": 0.1}}
	embedder := &mockEmbedder{}

	gk := NewGatekeeper(reg, defaultGatekeeperCfg(), WithEmbedder(embedder), WithSearcher(searcher))
	candidates, err := gk.FindCandidates(context.Background(), &domain.AuctionTask{ID: "t-l2-4", Description: "some task"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected 1 (prov), got %d: %v", len(candidates), candidateIDs(candidates))
	}
	if candidates[0].Agent.ID != "prov" {
		t.Errorf("expected 'prov', got %q", candidates[0].Agent.ID)
	}
	if candidates[0].Score != DefaultProvisionalScore {
		t.Errorf("expected DefaultProvisionalScore %.4f, got %.4f", 		DefaultProvisionalScore, candidates[0].Score)
	}
}

func TestFindCandidates_TraitDaemon_Excluded(t *testing.T) {
	daemon := domain.AgentDefinition{ID: "pulse", Provisional: false, Trait: domain.TraitDaemon}
	cog := domain.AgentDefinition{ID: "analyst", Provisional: false, Trait: domain.TraitCognitive}
	reg := newMockAgentDeclarationSource([]domain.AgentDefinition{daemon, cog}, nil)

	searcher := &fakeInterviewSearcher{results: map[string]float64{"pulse": 0.99, "analyst": 0.8}}
	embedder := &mockEmbedder{}

	gk := NewGatekeeper(reg, defaultGatekeeperCfg(), WithEmbedder(embedder), WithSearcher(searcher))
	candidates, err := gk.FindCandidates(context.Background(), &domain.AuctionTask{ID: "t-daemon", Description: "analysis task"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate (cog), got %d: %v", len(candidates), candidateIDs(candidates))
	}
	if candidates[0].Agent.ID != "analyst" {
		t.Errorf("expected 'analyst', got %q", candidates[0].Agent.ID)
	}
}
