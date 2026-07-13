package gatekeeper

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

type trackingInterviewSearcher struct {
	called bool
	inner  *fakeInterviewSearcher
}

func (t *trackingInterviewSearcher) SearchByEmbedding(ctx context.Context, embedding []float32, threshold float64, topK int) ([]domain.AgentSearchResult, error) {
	t.called = true
	return t.inner.SearchByEmbedding(ctx, embedding, threshold, topK)
}

func TestFindCandidates_TraitTool_GoesThroughLayer2(t *testing.T) {
	toolAgent := domain.AgentDefinition{ID: "tool-gk-1", Provisional: false, Trait: domain.TraitTool}
	registry := newMockAgentDeclarationSource([]domain.AgentDefinition{toolAgent}, nil)

	searcher := &trackingInterviewSearcher{inner: &fakeInterviewSearcher{results: map[string]float64{}}}
	embedder := &mockEmbedder{}

	gk := NewGatekeeper(registry, defaultGatekeeperCfg(), WithEmbedder(embedder), WithSearcher(searcher))
	candidates, err := gk.FindCandidates(context.Background(), &domain.AuctionTask{ID: "t-gk-1", Description: "do some work"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// ADR-0023: tool agents no longer bypass Layer 2; they must qualify in semantic search.
	if len(candidates) != 0 {
		t.Fatalf("expected 0 candidates (tool agent not in search results), got %d", len(candidates))
	}
	if !searcher.called {
		t.Error("SearchByEmbedding was NOT called for a TraitTool agent; it must be included in Layer 2")
	}
}

func TestFindCandidates_TraitTool_CognitiveAgentStillChecked(t *testing.T) {
	toolAgent := domain.AgentDefinition{ID: "tool-gk-2", Provisional: false, Trait: domain.TraitTool}
	cogAgent := domain.AgentDefinition{ID: "cog-gk-2", Provisional: false, Trait: domain.TraitCognitive}
	registry := newMockAgentDeclarationSource([]domain.AgentDefinition{toolAgent, cogAgent}, nil)

	searcher := &trackingInterviewSearcher{
		inner: &fakeInterviewSearcher{results: map[string]float64{cogAgent.ID: 0.8}},
	}
	embedder := &mockEmbedder{}

	gk := NewGatekeeper(registry, defaultGatekeeperCfg(), WithEmbedder(embedder), WithSearcher(searcher))
	candidates, err := gk.FindCandidates(context.Background(), &domain.AuctionTask{ID: "t-gk-2", Description: "cognitive task"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// ADR-0023: tool agents now go through Layer 2 like cognitive agents.
	// Only the cognitive agent qualifies in search results.
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate (cognitive only), got %d: %v", len(candidates), candidateIDs(candidates))
	}
	if candidates[0].Agent.ID != cogAgent.ID {
		t.Errorf("expected cognitive agent %q, got %q", cogAgent.ID, candidates[0].Agent.ID)
	}
	if !searcher.called {
		t.Error("expected SearchByEmbedding to be called for the cognitive agent")
	}
}

func TestFindCandidates_TraitTool_FailsDeclaration_Eliminated(t *testing.T) {
	toolAgent := domain.AgentDefinition{ID: "tool-gk-3", Provisional: false, Trait: domain.TraitTool}
	registry := newMockAgentDeclarationSource([]domain.AgentDefinition{toolAgent}, map[string]*domain.AgentManifest{
		toolAgent.ID: {SupportedFormats: []string{"csv"}},
	})

	searcher := &trackingInterviewSearcher{inner: &fakeInterviewSearcher{results: map[string]float64{}}}
	embedder := &mockEmbedder{}

	gk := NewGatekeeper(registry, defaultGatekeeperCfg(), WithEmbedder(embedder), WithSearcher(searcher))
	candidates, err := gk.FindCandidates(context.Background(), &domain.AuctionTask{
		ID:              "t-gk-3",
		Description:     "json task",
		RequiredFormats: []string{"json"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("expected 0 candidates, got %d", len(candidates))
	}
}
