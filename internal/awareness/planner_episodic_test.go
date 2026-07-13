package awareness

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ── test double: scripted WorkspaceStage ──────────────────────────────────────

type scriptedWorkspaceStage struct {
	enrichment domain.LTMEnrichment
}

func (s *scriptedWorkspaceStage) PrimeForPlanning(_ context.Context, _ string) (domain.LTMEnrichment, error) {
	return s.enrichment, nil
}
func (s *scriptedWorkspaceStage) PrimeForExecution(_ context.Context, _ *domain.ExecutionPlan, _ map[string]string) (map[string]string, error) {
	return nil, nil
}
func (s *scriptedWorkspaceStage) PrimeForStep(_ context.Context, _ string, _ []domain.ContextRef, _ []domain.SearchResult, _ float64, _ int) ([]domain.ContextRef, error) {
	return nil, nil
}

// ── test helpers ──────────────────────────────────────────────────────────────

func episodicSearchResult(sessionID, goal string, decisions []string) domain.SearchResult {
	now := time.Now()
	decisionObjs := make([]domain.Decision, len(decisions))
	for i, d := range decisions {
		decisionObjs[i] = domain.Decision{
			Text:            d,
			MadeAt:          now,
			SourceEventType: domain.EventUserMessage,
		}
	}
	em := domain.EpisodicMemory{
		SessionID:   sessionID,
		Goal:        goal,
		StartedAt:   now.Add(-time.Hour),
		CompletedAt: now,
		Decisions:   decisionObjs,
	}
	return domain.SearchResult{
		Score: 0.72,
		Document: domain.Document{
			ID:           "ep-" + sessionID,
			DocumentType: domain.DocTypeEpisodicMemory,
			Text:         goal + ": " + strings.Join(decisions, "; "),
			Metadata:     map[string]interface{}{"episodic": em},
		},
	}
}

func plannerWithEpisodicStage(enrichment domain.LTMEnrichment) *Planner {
	gen := &mockGenerator{response: minimalPlanJSON()}
	p := NewPlanner(gen, &mockAgentProvider{}, nil)
	p.WorkspaceStage = &scriptedWorkspaceStage{enrichment: enrichment}
	return p
}

// ── Cycle 1: non-empty Episodes → prompt contains <EpisodicMemory> ───────────

func TestPlanner_EpisodicMemoryBlock_WhenEpisodesPresent(t *testing.T) {
	enrichment := domain.LTMEnrichment{
		Episodes: []domain.SearchResult{
			episodicSearchResult("sess-001", "auth design", []string{"use JWT", "skip OAuth"}),
		},
	}
	p := plannerWithEpisodicStage(enrichment)

	_, err := p.GetExecutionPlan(context.Background(), "what did we decide about auth?")
	if err != nil {
		t.Fatalf("GetExecutionPlan: %v", err)
	}

	gen := p.client.(*mockGenerator)
	if len(gen.capturedPrompts) == 0 {
		t.Fatal("no prompt was captured")
	}
	prompt := gen.capturedPrompts[len(gen.capturedPrompts)-1]

	if !strings.Contains(prompt, "<EpisodicMemory>") {
		t.Errorf("expected <EpisodicMemory> tag in prompt; prompt excerpt:\n%s",
			prompt[:min(len(prompt), 500)])
	}
	if !strings.Contains(prompt, "use JWT") {
		t.Errorf("expected decision text 'use JWT' in episodic block; got prompt excerpt:\n%s",
			prompt[:min(len(prompt), 500)])
	}
}

// ── Cycle 2: empty Episodes → prompt does NOT contain <EpisodicMemory> ───────

func TestPlanner_NoEpisodicBlock_WhenEpisodesEmpty(t *testing.T) {
	enrichment := domain.LTMEnrichment{
		Episodes: nil,
	}
	p := plannerWithEpisodicStage(enrichment)

	_, err := p.GetExecutionPlan(context.Background(), "sort this array")
	if err != nil {
		t.Fatalf("GetExecutionPlan: %v", err)
	}

	gen := p.client.(*mockGenerator)
	prompt := gen.capturedPrompts[len(gen.capturedPrompts)-1]

	if strings.Contains(prompt, "<EpisodicMemory>") {
		t.Errorf("expected NO <EpisodicMemory> tag when Episodes is empty; found it in prompt")
	}
}

// ── Cycle 3: multiple episodes → all appear inside single <EpisodicMemory> ───

func TestPlanner_MultipleEpisodes_AllRendered(t *testing.T) {
	enrichment := domain.LTMEnrichment{
		Episodes: []domain.SearchResult{
			episodicSearchResult("sess-001", "auth design", []string{"use JWT"}),
			episodicSearchResult("sess-002", "db choice", []string{"use pgvector"}),
		},
	}
	p := plannerWithEpisodicStage(enrichment)

	_, err := p.GetExecutionPlan(context.Background(), "review past decisions")
	if err != nil {
		t.Fatalf("GetExecutionPlan: %v", err)
	}

	gen := p.client.(*mockGenerator)
	prompt := gen.capturedPrompts[len(gen.capturedPrompts)-1]

	openCount := strings.Count(prompt, "<EpisodicMemory>")
	closeCount := strings.Count(prompt, "</EpisodicMemory>")
	if openCount != 1 || closeCount != 1 {
		t.Errorf("expected exactly one <EpisodicMemory> block, got %d open / %d close tags", openCount, closeCount)
	}
	if !strings.Contains(prompt, "use JWT") {
		t.Errorf("episode 1 decision 'use JWT' missing from prompt")
	}
	if !strings.Contains(prompt, "use pgvector") {
		t.Errorf("episode 2 decision 'use pgvector' missing from prompt")
	}
}

// ── Cycle 4: malformed Metadata["episodic"] → episode skipped, no panic ──────

func TestPlanner_MalformedEpisodicMetadata_Skipped(t *testing.T) {
	malformed := domain.SearchResult{
		Score: 0.80,
		Document: domain.Document{
			ID:           "ep-bad",
			DocumentType: domain.DocTypeEpisodicMemory,
			Text:         "bad episode",
			Metadata:     map[string]interface{}{"episodic": "not-a-struct"},
		},
	}
	good := episodicSearchResult("sess-ok", "valid goal", []string{"valid decision"})
	enrichment := domain.LTMEnrichment{
		Episodes: []domain.SearchResult{malformed, good},
	}
	p := plannerWithEpisodicStage(enrichment)

	_, err := p.GetExecutionPlan(context.Background(), "what did we decide?")
	if err != nil {
		t.Fatalf("GetExecutionPlan should not fail on malformed metadata: %v", err)
	}

	gen := p.client.(*mockGenerator)
	prompt := gen.capturedPrompts[len(gen.capturedPrompts)-1]

	// Good episode must appear; no panic from the bad one
	if !strings.Contains(prompt, "valid decision") {
		t.Errorf("valid episode should appear in prompt even after malformed one is skipped")
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// verify buildLTMBlock renders EpisodicMemory from the typed metadata field
func TestBuildLTMBlock_EpisodicMemory_RendersCorrectXML(t *testing.T) {
	now := time.Now()
	em := domain.EpisodicMemory{
		SessionID:   "sess-xml",
		Goal:        "xml test",
		CompletedAt: now,
		Decisions: []domain.Decision{
			{Text: "use XML", SourceEventType: domain.EventUserMessage},
		},
	}
	result := episodicSearchResult("sess-xml", "xml test", []string{"use XML"})
	// Overwrite with a proper EpisodicMemory value (not a map)
	result.Document.Metadata["episodic"] = em

	block := buildLTMBlock(nil, domain.LTMEnrichment{Episodes: []domain.SearchResult{result}})

	if !strings.Contains(block, "<EpisodicMemory>") {
		t.Errorf("expected <EpisodicMemory> tag, got:\n%s", block)
	}
	if !strings.Contains(block, "xml test") {
		t.Errorf("expected goal 'xml test' in block, got:\n%s", block)
	}
	if !strings.Contains(block, "use XML") {
		t.Errorf("expected decision 'use XML' in block, got:\n%s", block)
	}
	// Ensure valid JSON is storable as Metadata (round-trip check)
	b, _ := json.Marshal(em)
	if len(b) == 0 {
		t.Error("EpisodicMemory should marshal to non-empty JSON")
	}
}

// ADR-0049 D11: the planner LTM block renders precedents (transitions) for the LLM to
// reason over — situation + outcome + the action path — as evidence, not routing.
func TestBuildLTMBlock_RendersPrecedents(t *testing.T) {
	enr := domain.LTMEnrichment{Precedents: []domain.Precedent{
		{
			SceneID:    "scene-p1",
			Situation:  "goal: deploy | engages: 1 web resource",
			Outcome:    "failure",
			Similarity: 0.82,
			Actions:    []string{"deploy → err: timeout"},
		},
	}}
	block := buildLTMBlock(nil, enr)

	for _, want := range []string{"<PrecedentLTM>", `outcome="failure"`, "goal: deploy", "deploy → err: timeout"} {
		if !strings.Contains(block, want) {
			t.Errorf("precedent block must contain %q; got:\n%s", want, block)
		}
	}
}
