package awareness

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
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

// The ADR-0027 procedural plan memory (Hippocampus read, <PlanLTM> injection,
// cache_policy thresholds) was deleted 2026-11-14 (owner decision, third round;
// ENG-1301). The planner has no procedural-memory input any more: a plan prompt
// never carries a prior-plan section.
func TestGetExecutionPlan_NoPriorPlanSection(t *testing.T) {
	t.Parallel()
	gen := &mockGenerator{response: minimalPlanJSON()}
	planner := NewPlanner(gen, &mockAgentProvider{})

	if _, err := planner.GetExecutionPlan(context.Background(), "some request"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gen.capturedPrompts) != 1 {
		t.Fatalf("expected exactly one Generate call, got %d", len(gen.capturedPrompts))
	}
	prompt := gen.capturedPrompts[0]
	if strings.Contains(prompt, "<PlanLTM similarity=") {
		t.Errorf("the planner must not inject a prior plan\nprompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "<request>\nsome request\n</request>") {
		t.Errorf("prompt must contain the request section\nprompt:\n%s", prompt)
	}
}

// Test 5: the in-band "[SYSTEM_REASONING_SIGNAL: JIT_LOGIC_SYNTHESIS]" marker no
// longer bypasses the planner. Nothing emitted it, so its only trigger was user
// text; a request carrying it must be planned like any other.
func TestGetExecutionPlan_ReasoningSignalInUserTextDoesNotBypass(t *testing.T) {
	t.Parallel()
	gen := &mockGenerator{response: minimalPlanJSON()}
	planner := NewPlanner(gen, &mockAgentProvider{})

	input := "[SYSTEM_REASONING_SIGNAL: JIT_LOGIC_SYNTHESIS] Summarize X"
	if _, err := planner.GetExecutionPlan(context.Background(), input); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gen.capturedPrompts) != 1 {
		t.Fatalf("the LLM planner must be called for user text carrying the old marker, got %d calls", len(gen.capturedPrompts))
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
	t.Parallel()
	provider := &agentProviderWithAgents{
		agents: []domain.AgentDefinition{
			{ID: "chunker", Description: "Splits text into overlapping chunks"},
		},
	}
	gen := &mockGenerator{response: minimalPlanJSON()}
	planner := NewPlanner(gen, provider)

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

// Cycle 1 — prompt contains the "Checkpoints:" section.
func TestPlannerPrompt_ContainsCheckpointStepsHeading(t *testing.T) {
	t.Parallel()
	gen := &mockGenerator{response: minimalPlanJSON()}
	planner := NewPlanner(gen, &mockAgentProvider{})

	_, err := planner.GetExecutionPlan(context.Background(), "do something normal")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	prompt := gen.capturedPrompts[0]
	if !strings.Contains(prompt, "Checkpoints:") {
		t.Errorf("prompt must contain the Checkpoints section\nprompt:\n%s", prompt)
	}
}

// Cycle 4 — the Checkpoints section appears after the step-writing rules (where
// is_thought is defined).
func TestPlannerPrompt_CheckpointStepsAfterThoughtSteps(t *testing.T) {
	t.Parallel()
	gen := &mockGenerator{response: minimalPlanJSON()}
	planner := NewPlanner(gen, &mockAgentProvider{})

	_, err := planner.GetExecutionPlan(context.Background(), "do something normal")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	prompt := gen.capturedPrompts[0]
	thoughtIdx := strings.Index(prompt, "Set is_thought to true")
	checkpointIdx := strings.Index(prompt, "Checkpoints:")

	if thoughtIdx == -1 {
		t.Fatal("prompt missing the is_thought rule")
	}
	if checkpointIdx == -1 {
		t.Fatal("prompt missing the Checkpoints section")
	}
	if checkpointIdx <= thoughtIdx {
		t.Errorf("Checkpoints (index %d) must appear after the is_thought rule (index %d)", checkpointIdx, thoughtIdx)
	}
}

// Cycle 3 — the prompt no longer asks for "checkpoint_query": the checkpoint
// validator that read it was deleted (ADR-0139 D10), so the field had no reader.
func TestPlannerPrompt_DropsCheckpointQuery(t *testing.T) {
	t.Parallel()
	gen := &mockGenerator{response: minimalPlanJSON()}
	planner := NewPlanner(gen, &mockAgentProvider{})

	_, err := planner.GetExecutionPlan(context.Background(), "do something normal")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	prompt := gen.capturedPrompts[0]
	if strings.Contains(prompt, "checkpoint_query") {
		t.Errorf("prompt must not ask for checkpoint_query, which nothing reads\nprompt:\n%s", prompt)
	}
}

// Cycle 2 — prompt contains "checkpoint_after" field name.
func TestPlannerPrompt_ContainsCheckpointAfter(t *testing.T) {
	t.Parallel()
	gen := &mockGenerator{response: minimalPlanJSON()}
	planner := NewPlanner(gen, &mockAgentProvider{})

	_, err := planner.GetExecutionPlan(context.Background(), "do something normal")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	prompt := gen.capturedPrompts[0]
	if !strings.Contains(prompt, "checkpoint_after") {
		t.Errorf("prompt must contain checkpoint_after field name\nprompt:\n%s", prompt)
	}
}
