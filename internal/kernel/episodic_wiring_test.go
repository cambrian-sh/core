package kernel_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/internal/awareness"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/memory"
)

// ── in-memory VectorStore ────────────────────────────────────────────────────

type inMemoryVecStore struct {
	mu   sync.Mutex
	docs map[string]*domain.Document
}

func newInMemoryVecStore() *inMemoryVecStore {
	return &inMemoryVecStore{docs: make(map[string]*domain.Document)}
}

func (s *inMemoryVecStore) Save(_ context.Context, doc *domain.Document) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.docs[doc.ID] = doc
	return nil
}
func (s *inMemoryVecStore) SaveBatch(_ context.Context, docs []*domain.Document) error {
	for _, d := range docs {
		_ = s.Save(context.Background(), d)
	}
	return nil
}
func (s *inMemoryVecStore) Search(_ context.Context, _ []float32, opts domain.SearchOptions) ([]domain.SearchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.SearchResult
	for _, doc := range s.docs {
		if opts.DocumentType != "" && doc.DocumentType != opts.DocumentType {
			continue
		}
		out = append(out, domain.SearchResult{Document: *doc, Score: 0.80})
	}
	return out, nil
}
func (s *inMemoryVecStore) GetByID(_ context.Context, id string) (*domain.Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.docs[id], nil
}
func (s *inMemoryVecStore) GetBatch(_ context.Context, ids []string) ([]domain.Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.Document
	for _, id := range ids {
		if d, ok := s.docs[id]; ok {
			out = append(out, *d)
		}
	}
	return out, nil
}
func (s *inMemoryVecStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.docs, id)
	return nil
}
func (s *inMemoryVecStore) DeleteBatch(_ context.Context, ids []string) error {
	for _, id := range ids {
		_ = s.Delete(context.Background(), id)
	}
	return nil
}
func (s *inMemoryVecStore) IncrementAccess(_ context.Context, _ string) error { return nil }
func (s *inMemoryVecStore) GetStaleMemories(_ context.Context, _ int) ([]domain.Document, error) {
	return nil, nil
}
func (s *inMemoryVecStore) QueryByMetadata(_ context.Context, _ map[string]string, _ int) ([]domain.Document, error) {
	return nil, nil
}

// ── fake embedder (returns unit vector) ─────────────────────────────────────

type unitEmbedder struct{}

func (u *unitEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return []float32{1.0}, nil
}

// ── fake LLM generators ──────────────────────────────────────────────────────

type episodicFakeLLM struct {
	response string
}

func (g *episodicFakeLLM) Generate(_ context.Context, _ string) (string, error) {
	return g.response, nil
}

func episodicLLMResp() string {
	type d struct {
		Text            string `json:"text"`
		MadeAt          string `json:"made_at"`
		SourceEventType string `json:"source_event_type"`
	}
	type a struct{ Text string `json:"text"` }
	type out struct {
		Decisions   []d `json:"decisions"`
		ActionItems []a `json:"action_items"`
	}
	b, _ := json.Marshal(out{
		Decisions:   []d{{Text: "use JWT for auth", MadeAt: time.Now().Format(time.RFC3339), SourceEventType: "user_message"}},
		ActionItems: []a{{Text: "implement JWT middleware"}},
	})
	return string(b)
}

func minimalPlanResp() string {
	b, _ := json.Marshal(domain.ExecutionPlan{
		Subject: "auth query",
		Steps:   []domain.Step{{Query: "step one", DependsOn: []int{}}},
	})
	return string(b)
}

// ── Cycle 1: full episodic path — ExtractAndSave → PrimeForPlanning → Planner ──

func TestEpisodicWiring_FullPath(t *testing.T) {
	ctx := context.Background()
	store := newInMemoryVecStore()

	// 1. EpisodicExtractor saves an EpisodicMemory document to the store.
	ex := awareness.NewEpisodicExtractor(
		&episodicFakeLLM{response: episodicLLMResp()},
		store,
		domain.NewRegexPIIMasker(),
	)
	sess := domain.Session{
		ID:          "sess-wire-001",
		Goal:        "auth flow design",
		Status:      domain.SessionCompleted,
		CreatedAt:   time.Now().Add(-2 * time.Hour),
		CompletedAt: time.Now(),
		UpdatedAt:   time.Now(),
	}
	err := ex.ExtractAndSave(ctx, awareness.EpisodicExtractionInput{
		Session: sess,
		Events: []domain.SessionEvent{
			{SessionID: "sess-wire-001", Type: domain.EventUserMessage, Payload: "we discussed auth"},
		},
	})
	if err != nil {
		t.Fatalf("ExtractAndSave: %v", err)
	}

	// 2. WorkspaceStage with PolicyProvider retrieves the episodic document.
	pp := config.NewStaticPolicyProvider(
		map[string]domain.HippocampusPolicy{
			"episodic": {SimilarityThreshold: 0.50, MaxAgeHours: 8760},
		},
		"episodic",
	)
	ws := memory.NewWorkspaceStage(store, &unitEmbedder{}, nil, 10, 5, 0.2, false, 0.7)
	ws.PolicyProvider = pp

	enrichment, err := ws.PrimeForPlanning(ctx, "what did we decide about auth?")
	if err != nil {
		t.Fatalf("PrimeForPlanning: %v", err)
	}
	if len(enrichment.Episodes) == 0 {
		t.Fatal("expected at least one Episode in LTMEnrichment.Episodes after ExtractAndSave")
	}

	// 3. Planner injects <EpisodicMemory> into the prompt.
	planLLM := &episodicFakeLLM{response: minimalPlanResp()}
	planner := awareness.NewPlanner(planLLM, &emptyAgentProvider{}, nil)
	planner.WorkspaceStage = &fixedWorkspaceStage{enrichment: enrichment}

	_, err = planner.GetExecutionPlan(ctx, "what did we decide about auth?")
	if err != nil {
		t.Fatalf("GetExecutionPlan: %v", err)
	}

	// Full path verified: no error means ExtractAndSave → PrimeForPlanning → GetExecutionPlan
	// all executed without panicking. Prompt content is verified in the next test.
}

// TestEpisodicWiring_PlannerPromptContainsEpisodicBlock drives through the
// Planner's prompt generation path directly, confirming the XML block is present.
func TestEpisodicWiring_PlannerPromptContainsEpisodicBlock(t *testing.T) {
	ctx := context.Background()
	store := newInMemoryVecStore()

	ex := awareness.NewEpisodicExtractor(
		&episodicFakeLLM{response: episodicLLMResp()},
		store,
		domain.NewRegexPIIMasker(),
	)
	err := ex.ExtractAndSave(ctx, awareness.EpisodicExtractionInput{
		Session: domain.Session{
			ID:          "sess-prompt-001",
			Goal:        "JWT authentication design",
			Status:      domain.SessionCompleted,
			CreatedAt:   time.Now().Add(-time.Hour),
			CompletedAt: time.Now(),
			UpdatedAt:   time.Now(),
		},
		Events: []domain.SessionEvent{
			{SessionID: "sess-prompt-001", Type: domain.EventUserMessage, Payload: "auth discussion"},
		},
	})
	if err != nil {
		t.Fatalf("ExtractAndSave: %v", err)
	}

	pp := config.NewStaticPolicyProvider(
		map[string]domain.HippocampusPolicy{
			"episodic": {SimilarityThreshold: 0.50, MaxAgeHours: 8760},
		},
		"episodic",
	)
	ws := memory.NewWorkspaceStage(store, &unitEmbedder{}, nil, 10, 5, 0.2, false, 0.7)
	ws.PolicyProvider = pp

	// Capture LLM prompt
	capLLM := &promptCapturingLLM{response: minimalPlanResp()}
	planner := awareness.NewPlanner(capLLM, &emptyAgentProvider{}, nil)
	planner.WorkspaceStage = ws

	_, err = planner.GetExecutionPlan(ctx, "what did we decide about auth?")
	if err != nil {
		t.Fatalf("GetExecutionPlan: %v", err)
	}

	if len(capLLM.prompts) == 0 {
		t.Fatal("no prompt captured")
	}
	prompt := capLLM.prompts[len(capLLM.prompts)-1]
	if !strings.Contains(prompt, "<EpisodicMemory>") {
		t.Errorf("expected <EpisodicMemory> in Planner prompt; prompt:\n%s", prompt[:min(len(prompt), 600)])
	}
	if !strings.Contains(prompt, "use JWT for auth") {
		t.Errorf("expected decision text in prompt; prompt:\n%s", prompt[:min(len(prompt), 600)])
	}
}

// ── Cycle 2: unrelated query → no <EpisodicMemory> in prompt ─────────────────

func TestEpisodicWiring_UnrelatedQuery_NoEpisodicBlock(t *testing.T) {
	ctx := context.Background()
	store := newInMemoryVecStore()

	ex := awareness.NewEpisodicExtractor(
		&episodicFakeLLM{response: episodicLLMResp()},
		store,
		domain.NewRegexPIIMasker(),
	)
	_ = ex.ExtractAndSave(ctx, awareness.EpisodicExtractionInput{
		Session: domain.Session{
			ID:          "sess-unrelated",
			Goal:        "JWT authentication design",
			Status:      domain.SessionCompleted,
			CreatedAt:   time.Now().Add(-time.Hour),
			CompletedAt: time.Now(),
			UpdatedAt:   time.Now(),
		},
		Events: []domain.SessionEvent{
			{SessionID: "sess-unrelated", Type: domain.EventUserMessage, Payload: "auth discussion"},
		},
	})

	// Policy with very HIGH threshold — nothing will clear it for unrelated queries
	pp := config.NewStaticPolicyProvider(
		map[string]domain.HippocampusPolicy{
			"episodic": {SimilarityThreshold: 0.99, MaxAgeHours: 8760},
		},
		"episodic",
	)
	ws := memory.NewWorkspaceStage(store, &unitEmbedder{}, nil, 10, 5, 0.2, false, 0.7)
	ws.PolicyProvider = pp

	capLLM := &promptCapturingLLM{response: minimalPlanResp()}
	planner := awareness.NewPlanner(capLLM, &emptyAgentProvider{}, nil)
	planner.WorkspaceStage = ws

	_, _ = planner.GetExecutionPlan(ctx, "sort this array quickly")

	if len(capLLM.prompts) > 0 {
		prompt := capLLM.prompts[len(capLLM.prompts)-1]
		if strings.Contains(prompt, "<EpisodicMemory>") {
			t.Errorf("expected NO <EpisodicMemory> for unrelated query with high threshold")
		}
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

type promptCapturingLLM struct {
	response string
	prompts  []string
}

func (p *promptCapturingLLM) Generate(_ context.Context, prompt string) (string, error) {
	p.prompts = append(p.prompts, prompt)
	return p.response, nil
}

type emptyAgentProvider struct{}

func (e *emptyAgentProvider) GetAllAgents(_ context.Context) ([]domain.AgentDefinition, error) {
	return nil, nil
}
func (e *emptyAgentProvider) GetManifest(_ context.Context, _ string) (*domain.AgentManifest, error) {
	return nil, nil
}

type fixedWorkspaceStage struct {
	enrichment domain.LTMEnrichment
}

func (f *fixedWorkspaceStage) PrimeForPlanning(_ context.Context, _ string) (domain.LTMEnrichment, error) {
	return f.enrichment, nil
}
func (f *fixedWorkspaceStage) PrimeForExecution(_ context.Context, _ *domain.ExecutionPlan, _ map[string]string) (map[string]string, error) {
	return nil, nil
}
func (f *fixedWorkspaceStage) PrimeForStep(_ context.Context, _ string, _ []domain.ContextRef, _ []domain.SearchResult, _ float64, _ int) ([]domain.ContextRef, error) {
	return nil, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
