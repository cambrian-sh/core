package awareness

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/memory"
)

// ------------------------------------------------------------
// Test doubles
// ------------------------------------------------------------

// mockGenerator captures the prompt passed to Generate and returns a preset
// JSON ExecutionPlan response.
type mockGenerator struct {
	capturedPrompts []string
	response        string
	err             error
}

func (m *mockGenerator) Generate(_ context.Context, prompt string) (string, error) {
	m.capturedPrompts = append(m.capturedPrompts, prompt)
	if m.err != nil {
		return "", m.err
	}
	return m.response, nil
}

// mockAgentProvider returns an empty agent list so the Planner's agent
// description section is empty. This is sufficient for testing prompt
// injection behaviour without needing real agents.
type mockAgentProvider struct{}

func (m *mockAgentProvider) GetAllAgents(_ context.Context) ([]domain.AgentDefinition, error) {
	return []domain.AgentDefinition{}, nil
}

func (m *mockAgentProvider) GetManifest(_ context.Context, _ string) (*domain.AgentManifest, error) {
	return nil, fmt.Errorf("no manifest")
}

// ------------------------------------------------------------
// Helper — build a minimal valid LLM response for a plan.
// ------------------------------------------------------------

func minimalPlanJSON() string {
	plan := domain.ExecutionPlan{
		Subject: "test",
		Steps: []domain.Step{
			{Query: "step one", DependsOn: []int{}},
		},
	}
	b, _ := json.Marshal(plan)
	return string(b)
}

// priorPlan returns the preset ExecutionPlan used as the "prior successful
// plan" in hippocampus injection tests.
func priorPlan() *domain.ExecutionPlan {
	return &domain.ExecutionPlan{
		Subject: "prior",
		Steps: []domain.Step{
			{Query: "prior step", DependsOn: []int{}},
		},
	}
}

// ------------------------------------------------------------
// plannerWithMockHippocampus creates a Planner wired with a mock hippocampus
// that returns the given plan and confidence. The hippocampus is constructed
// by composing a real *Hippocampus with mock dependencies that return the
// preset values.
// ------------------------------------------------------------

// zeroEmbedder always returns a zero vector (distinct from fakeEmbedder in hippocampus_test.go).
type zeroEmbedder struct{}

func (z *zeroEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return []float32{0.0}, nil
}

// plannerFakePlanStore returns a preset search result on Search, or empty
// results when preset is nil. Save is a no-op.
type plannerFakePlanStore struct {
	preset *domain.ExecutionPlan
	conf   float64
	score  float64
}

func (f *plannerFakePlanStore) Save(_ context.Context, _ *domain.Document) error        { return nil }
func (f *plannerFakePlanStore) SaveBatch(_ context.Context, _ []*domain.Document) error { return nil }
func (f *plannerFakePlanStore) Delete(_ context.Context, _ string) error                { return nil }
func (f *plannerFakePlanStore) GetByID(_ context.Context, _ string) (*domain.Document, error) {
	return nil, nil
}
func (f *plannerFakePlanStore) GetBatch(_ context.Context, _ []string) ([]domain.Document, error) {
	return nil, nil
}
func (f *plannerFakePlanStore) DeleteBatch(_ context.Context, _ []string) error   { return nil }
func (f *plannerFakePlanStore) IncrementAccess(_ context.Context, _ string) error { return nil }
func (f *plannerFakePlanStore) GetStaleMemories(_ context.Context, _ int) ([]domain.Document, error) {
	return nil, nil
}
func (f *plannerFakePlanStore) QueryByMetadata(_ context.Context, _ map[string]string, _ int) ([]domain.Document, error) {
	return nil, nil
}

func (s *plannerFakePlanStore) Search(_ context.Context, _ []float32, _ domain.SearchOptions) ([]domain.SearchResult, error) {
	envelope := memory.ProceduralTemplateV1{
		Version: 1,
		Plan:    s.preset,
	}
	data, _ := json.Marshal(envelope)

	md := map[string]interface{}{
		"mean_auction_confidence": s.conf,
	}
	if s.preset != nil && s.preset.PlannerPromptVersion != "" {
		md["planner_prompt_version"] = s.preset.PlannerPromptVersion
	}
	return []domain.SearchResult{
		{
			Score: s.score,
			Document: domain.Document{
				Text:     string(data),
				Metadata: md,
			},
		},
	}, nil
}

// newTestPlanner is a convenience builder for tests that control the
// hippocampus behaviour through the store score/threshold.
func newTestPlanner(gen Generator, hippoStore *plannerFakePlanStore) *Planner {
	var h domain.ProceduralMemory
	if hippoStore != nil {
		h = memory.NewHippocampus(hippoStore, &zeroEmbedder{}, nil)
	}
	return NewPlanner(gen, &mockAgentProvider{}, h)
}

// ------------------------------------------------------------
// Tests
// ------------------------------------------------------------

// Test 1: When the hippocampus returns a non-nil plan, the prompt passed to
// Generator.Generate contains the PRIOR SUCCESSFUL PLAN marker with the plan
// JSON and the confidence formatted to two decimal places.
func TestGetExecutionPlan_HippocampusInjectsPriorPlan(t *testing.T) {
	prior := priorPlan()
	conf := 0.876

	// score 0.9 is above the 0.85 similarity threshold
	store := &plannerFakePlanStore{preset: prior, conf: conf, score: 0.9}

	gen := &mockGenerator{response: minimalPlanJSON()}
	planner := newTestPlanner(gen, store)

	_, err := planner.GetExecutionPlan(context.Background(), "find something in the database")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(gen.capturedPrompts) == 0 {
		t.Fatal("expected at least one Generate call")
	}
	prompt := gen.capturedPrompts[0]

	// Must contain the <PlanLTM marker (ADR-0025 format).
	if !strings.Contains(prompt, "<PlanLTM") {
		t.Errorf("prompt missing <PlanLTM marker\nprompt:\n%s", prompt)
	}

	// Must contain confidence formatted to two decimal places (0.88)
	expectedConfStr := fmt.Sprintf("%.2f", conf)
	if !strings.Contains(prompt, expectedConfStr) {
		t.Errorf("prompt missing confidence %q\nprompt:\n%s", expectedConfStr, prompt)
	}

	// Must contain the prior plan JSON
	priorJSON, _ := json.Marshal(prior)
	if !strings.Contains(prompt, string(priorJSON)) {
		t.Errorf("prompt missing prior plan JSON\nprompt:\n%s", prompt)
	}

	// PLANNERREQ REQ4: <PlanLTM block must appear BEFORE "User Request:" (context before instruction).
	planIdx := strings.Index(prompt, "<PlanLTM similarity=")
	userIdx := strings.Index(prompt, "User Request:")
	if userIdx == -1 {
		t.Error("prompt missing User Request section")
	} else if planIdx == -1 {
		t.Error("prompt missing <PlanLTM similarity= marker")
	} else if planIdx > userIdx {
		t.Error("<PlanLTM must appear BEFORE User Request: (REQ4: context before instruction)")
	}
}

// Test 2: When the hippocampus returns nil (no template found — score below
// threshold), the prompt is unchanged from the baseline (no PRIOR SUCCESSFUL
// PLAN section).
func TestGetExecutionPlan_HippocampusNoTemplate_PromptUnchanged(t *testing.T) {
	// score 0.1 is below the 0.85 threshold — Retrieve will return nil
	store := &plannerFakePlanStore{preset: priorPlan(), conf: 0.9, score: 0.1}

	gen := &mockGenerator{response: minimalPlanJSON()}
	planner := newTestPlanner(gen, store)

	_, err := planner.GetExecutionPlan(context.Background(), "some request")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(gen.capturedPrompts) == 0 {
		t.Fatal("expected at least one Generate call")
	}
	prompt := gen.capturedPrompts[0]

	if strings.Contains(prompt, "PRIOR SUCCESSFUL PLAN") {
		t.Errorf("prompt must NOT contain PRIOR SUCCESSFUL PLAN when no template qualifies\nprompt:\n%s", prompt)
	}

	if !strings.Contains(prompt, "User Request:") {
		t.Errorf("prompt must still contain User Request section\nprompt:\n%s", prompt)
	}
}

// Test 3: When planner.hippocampus is nil, GetExecutionPlan behaves identically
// to its pre-hippocampus behaviour — no PRIOR SUCCESSFUL PLAN section.
func TestGetExecutionPlan_NilHippocampus_NoInjection(t *testing.T) {
	gen := &mockGenerator{response: minimalPlanJSON()}
	// Pass nil for hippocampus — no procedural memory
	planner := NewPlanner(gen, &mockAgentProvider{}, nil)

	_, err := planner.GetExecutionPlan(context.Background(), "some request")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(gen.capturedPrompts) == 0 {
		t.Fatal("expected at least one Generate call")
	}
	prompt := gen.capturedPrompts[0]

	if strings.Contains(prompt, "PRIOR SUCCESSFUL PLAN") {
		t.Errorf("nil hippocampus must not inject PRIOR SUCCESSFUL PLAN section\nprompt:\n%s", prompt)
	}

	if !strings.Contains(prompt, "User Request:") {
		t.Errorf("prompt must contain User Request section\nprompt:\n%s", prompt)
	}
}

// Test 4: Injected plan JSON round-trips back to *domain.ExecutionPlan.
// This validates the acceptance criterion that the injected plan JSON is valid
// and deserialisable.
func TestGetExecutionPlan_InjectedPlanJSONIsDeserializable(t *testing.T) {
	prior := priorPlan()
	// score 0.95 is above 0.85 threshold; conf 0.75 is above 0.5 floor
	store := &plannerFakePlanStore{preset: prior, conf: 0.75, score: 0.95}

	gen := &mockGenerator{response: minimalPlanJSON()}
	planner := newTestPlanner(gen, store)

	_, err := planner.GetExecutionPlan(context.Background(), "query")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	prompt := gen.capturedPrompts[0]

	// Extract the JSON block from the <PlanLTM similarity=...> tag (REQ4: tag before User Request).
	// Use "similarity=" to distinguish the actual tag from the "<PlanLTM>" reference in LTM CONTEXT RULES.
	if !strings.Contains(prompt, "<PlanLTM similarity=") {
		t.Fatal("PRIOR SUCCESSFUL PLAN marker not found in prompt (expected <PlanLTM similarity=)")
	}
	_, afterOpen, _ := strings.Cut(prompt, "<PlanLTM similarity=")
	_, jsonAndClose, _ := strings.Cut(afterOpen, "\n") // skip past the tag attribute line
	extractedJSON, _, _ := strings.Cut(strings.TrimSpace(jsonAndClose), "\n</PlanLTM>")
	extractedJSON = strings.TrimSpace(extractedJSON)

	var roundTripped domain.ExecutionPlan
	if err := json.Unmarshal([]byte(extractedJSON), &roundTripped); err != nil {
		t.Errorf("injected JSON does not deserialise to ExecutionPlan: %v\njson: %s", err, extractedJSON)
	}

	if roundTripped.Subject != prior.Subject {
		t.Errorf("round-tripped plan subject = %q, want %q", roundTripped.Subject, prior.Subject)
	}
}

// Test 4b: REQ-CACHE-1 exact-match cache hit — similarity >= 0.95, confidence >= 0.90,
// and PlannerPromptVersion matches current hash. LLM must NOT be called.
func TestGetExecutionPlan_ExactMatchCacheHit(t *testing.T) {
	prior := priorPlan()
	prior.PlannerPromptVersion = plannerPromptHash // must match current template hash

	// score 0.96 >= 0.95 threshold; conf 0.92 >= 0.90 floor
	store := &plannerFakePlanStore{preset: prior, conf: 0.92, score: 0.96}

	gen := &mockGenerator{response: "SHOULD NOT BE CALLED"}
	planner := newTestPlanner(gen, store)

	plan, err := planner.GetExecutionPlan(context.Background(), "query")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(gen.capturedPrompts) != 0 {
		t.Error("LLM should NOT be called for exact-match cache hit")
	}

	if plan == nil {
		t.Fatal("expected non-nil plan from cache")
	}
	if plan.Subject != prior.Subject {
		t.Errorf("cached plan subject = %q, want %q", plan.Subject, prior.Subject)
	}
	if plan.PlannerPromptVersion != plannerPromptHash {
		t.Errorf("cached plan prompt version = %q, want %q", plan.PlannerPromptVersion, plannerPromptHash)
	}

	// Verify it's a deep clone — mutation must not affect the original
	plan.Subject = "MUTATED"
	if prior.Subject == "MUTATED" {
		t.Error("cache returned a reference, not a deep clone")
	}
}

// Test 4c: REQ-CACHE-1 near-miss — similarity 0.94 (below 0.95) should fall through to LLM.
func TestGetExecutionPlan_NearMiss_FallsThroughToLLM(t *testing.T) {
	prior := priorPlan()
	prior.PlannerPromptVersion = plannerPromptHash

	// score 0.94 is close but below 0.95 exact-match threshold
	store := &plannerFakePlanStore{preset: prior, conf: 0.92, score: 0.94}

	gen := &mockGenerator{response: minimalPlanJSON()}
	planner := newTestPlanner(gen, store)

	_, err := planner.GetExecutionPlan(context.Background(), "query")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(gen.capturedPrompts) == 0 {
		t.Error("LLM SHOULD be called when similarity is below exact-match threshold")
	}
}

// Test 5: Fast-path for explicit JIT reasoning signal.
func TestGetExecutionPlan_ThoughtSignalFastPath(t *testing.T) {
	gen := &mockGenerator{response: "SHOULD NOT BE CALLED"}
	planner := NewPlanner(gen, &mockAgentProvider{}, nil)

	input := "[SYSTEM_REASONING_SIGNAL: JIT_LOGIC_SYNTHESIS] Summarize X"
	plan, err := planner.GetExecutionPlan(context.Background(), input)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(gen.capturedPrompts) != 0 {
		t.Error("LLM should NOT be called for JIT reasoning signal")
	}

	if len(plan.Steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(plan.Steps))
	}

	if !plan.Steps[0].IsThought {
		t.Error("expected IsThought=true for fast-path reasoning step")
	}

	if plan.Steps[0].Query != input {
		t.Errorf("expected Query=%q, got %q", input, plan.Steps[0].Query)
	}
}

// ── Capability vocabulary tests ───────────────────────────────────────────────

// agentProviderWithAgents is a test double that returns a fixed agent list
// with pre-set descriptions (no manifests needed — description-only vocabulary).
type agentProviderWithAgents struct {
	agents []domain.AgentDefinition
}

func (p *agentProviderWithAgents) GetAllAgents(_ context.Context) ([]domain.AgentDefinition, error) {
	return p.agents, nil
}

func (p *agentProviderWithAgents) GetManifest(_ context.Context, _ string) (*domain.AgentManifest, error) {
	return nil, fmt.Errorf("no manifest")
}

// Cycle 2 — prompt contains no required_tools instructions.
func TestPlannerPrompt_NoRequiredToolsInstructions(t *testing.T) {
	provider := &agentProviderWithAgents{
		agents: []domain.AgentDefinition{
			{ID: "chunker", Description: "Splits text into overlapping chunks"},
		},
	}
	gen := &mockGenerator{response: minimalPlanJSON()}
	planner := NewPlanner(gen, provider, nil)

	_, err := planner.GetExecutionPlan(context.Background(), "chunk the document")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	prompt := gen.capturedPrompts[0]

	if strings.Contains(prompt, "required_tools") {
		t.Errorf("prompt must NOT contain required_tools anywhere\nprompt:\n%s", prompt)
	}
}

// ── CHECKPOINT STEPS section tests (issue 0013-05) ───────────────────────────

// Cycle 1 — prompt contains "CHECKPOINT STEPS" heading.
func TestPlannerPrompt_ContainsCheckpointStepsHeading(t *testing.T) {
	gen := &mockGenerator{response: minimalPlanJSON()}
	planner := NewPlanner(gen, &mockAgentProvider{}, nil)

	_, err := planner.GetExecutionPlan(context.Background(), "do something normal")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	prompt := gen.capturedPrompts[0]
	if !strings.Contains(prompt, "CHECKPOINT STEPS") {
		t.Errorf("prompt must contain CHECKPOINT STEPS heading\nprompt:\n%s", prompt)
	}
}

// Cycle 4 — CHECKPOINT STEPS section appears after THOUGHT STEPS section.
func TestPlannerPrompt_CheckpointStepsAfterThoughtSteps(t *testing.T) {
	gen := &mockGenerator{response: minimalPlanJSON()}
	planner := NewPlanner(gen, &mockAgentProvider{}, nil)

	_, err := planner.GetExecutionPlan(context.Background(), "do something normal")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	prompt := gen.capturedPrompts[0]
	thoughtIdx := strings.Index(prompt, "THOUGHT STEPS")
	checkpointIdx := strings.Index(prompt, "CHECKPOINT STEPS")

	if thoughtIdx == -1 {
		t.Fatal("prompt missing THOUGHT STEPS section")
	}
	if checkpointIdx == -1 {
		t.Fatal("prompt missing CHECKPOINT STEPS section")
	}
	if checkpointIdx <= thoughtIdx {
		t.Errorf("CHECKPOINT STEPS (index %d) must appear after THOUGHT STEPS (index %d)", checkpointIdx, thoughtIdx)
	}
}

// Cycle 3 — prompt contains "checkpoint_query" field name.
func TestPlannerPrompt_ContainsCheckpointQuery(t *testing.T) {
	gen := &mockGenerator{response: minimalPlanJSON()}
	planner := NewPlanner(gen, &mockAgentProvider{}, nil)

	_, err := planner.GetExecutionPlan(context.Background(), "do something normal")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	prompt := gen.capturedPrompts[0]
	if !strings.Contains(prompt, "checkpoint_query") {
		t.Errorf("prompt must contain checkpoint_query field name\nprompt:\n%s", prompt)
	}
}

// Cycle 2 — prompt contains "checkpoint_after" field name.
func TestPlannerPrompt_ContainsCheckpointAfter(t *testing.T) {
	gen := &mockGenerator{response: minimalPlanJSON()}
	planner := NewPlanner(gen, &mockAgentProvider{}, nil)

	_, err := planner.GetExecutionPlan(context.Background(), "do something normal")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	prompt := gen.capturedPrompts[0]
	if !strings.Contains(prompt, "checkpoint_after") {
		t.Errorf("prompt must contain checkpoint_after field name\nprompt:\n%s", prompt)
	}
}

