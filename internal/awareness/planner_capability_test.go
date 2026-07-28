package awareness

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ROUTE-03: the capability_contract arm is a prompt/schema variant selected by
// SetCapabilityContract. OFF must be byte-identical to the pre-ROUTE-03 planner
// (same prompt text, same hash); ON must inject the capability rules + schema,
// stamp the distinct hash, and parse required_capabilities into the plan.

func capPlanJSON() string {
	plan := domain.ExecutionPlan{
		Subject: "test",
		Steps: []domain.Step{
			{Query: "read the file", DependsOn: []int{}, RequiredCapabilities: []string{"file_read"}},
		},
	}
	b, _ := json.Marshal(plan)
	return string(b)
}

func TestPlanner_CapabilityContract_OffIsByteIdentical(t *testing.T) {
	gen := &mockGenerator{response: minimalPlanJSON()}
	p := NewPlanner(gen, &mockAgentProvider{}, nil) // contract defaults OFF

	plan, err := p.GetExecutionPlan(context.Background(), "do something")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gen.capturedPrompts) != 1 {
		t.Fatalf("expected one prompt, got %d", len(gen.capturedPrompts))
	}
	prompt := gen.capturedPrompts[0]
	if strings.Contains(prompt, "CAPABILITY REQUIREMENTS") {
		t.Error("control-arm prompt must NOT contain the capability rules block")
	}
	if strings.Contains(prompt, "required_capabilities") {
		t.Error("control-arm prompt/schema must NOT mention required_capabilities")
	}
	if plan.PlannerPromptVersion != plannerPromptHash {
		t.Errorf("control-arm PlannerPromptVersion = %q, want the base hash %q",
			plan.PlannerPromptVersion, plannerPromptHash)
	}
}

func TestPlanner_CapabilityContract_OnEmitsAndParses(t *testing.T) {
	gen := &mockGenerator{response: capPlanJSON()}
	p := NewPlanner(gen, &mockAgentProvider{}, nil)
	p.SetCapabilityContract(true)

	plan, err := p.GetExecutionPlan(context.Background(), "read a file")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	prompt := gen.capturedPrompts[0]
	if !strings.Contains(prompt, "CAPABILITY REQUIREMENTS") {
		t.Error("capability-arm prompt must contain the capability rules block")
	}
	if !strings.Contains(prompt, "required_capabilities") {
		t.Error("capability-arm schema must mention required_capabilities")
	}
	if plan.PlannerPromptVersion != plannerPromptHashCap {
		t.Errorf("capability-arm PlannerPromptVersion = %q, want the cap hash %q",
			plan.PlannerPromptVersion, plannerPromptHashCap)
	}
	if len(plan.Steps) != 1 || len(plan.Steps[0].RequiredCapabilities) != 1 ||
		plan.Steps[0].RequiredCapabilities[0] != "file_read" {
		t.Errorf("expected parsed RequiredCapabilities [file_read], got %+v", plan.Steps)
	}
}

func TestPlanner_CapabilityContract_HashesDiffer(t *testing.T) {
	if plannerPromptHash == plannerPromptHashCap {
		t.Fatal("capability-arm prompt hash must differ from the base hash (provenance)")
	}
	// Both must be registered so PlanEvent lookups resolve.
	if _, ok := domain.PromptRegistry[plannerPromptHashCap]; !ok {
		t.Error("capability-arm prompt hash not registered in PromptRegistry")
	}
}

// The planner must be able to EXPRESS a pin, in both arms. Without the schema
// field the model has nowhere to put an agent name the user asked for, which is
// how `terminal_agent` ended up in required_capabilities and killed the step.
func TestPlannerSchemas_CarryAgentPinFields(t *testing.T) {
	for name, schema := range map[string]string{
		"base":       planOutputSchema,
		"capability": planOutputSchemaCap,
	} {
		if !strings.Contains(schema, "preferred_agent") {
			t.Errorf("%s schema must expose preferred_agent", name)
		}
		if !strings.Contains(schema, "agent_pin") {
			t.Errorf("%s schema must expose agent_pin", name)
		}
	}
	for name, text := range map[string]string{
		"base":       plannerStaticText,
		"capability": plannerStaticTextCap,
	} {
		if !strings.Contains(text, "AGENT PINNING") {
			t.Errorf("%s prompt must carry the pinning rules", name)
		}
	}
	// The instruction that caused the leak must be gone, not merely supplemented.
	if strings.Contains(plannerStaticText, "You may reference a specific agent ID") {
		t.Error("the old free-form agent-ID invitation must not survive")
	}
}

// vocabProvider declares a real capability vocabulary, unlike mockAgentProvider which
// has no agents at all. The guard only engages when the vocabulary is non-empty, so
// testing it requires a provider that actually knows something.
type vocabProvider struct{ caps []string }

func (v *vocabProvider) GetAllAgents(_ context.Context) ([]domain.AgentDefinition, error) {
	return []domain.AgentDefinition{{ID: "worker_agent"}}, nil
}

func (v *vocabProvider) GetManifest(_ context.Context, _ string) (*domain.AgentManifest, error) {
	return &domain.AgentManifest{Capabilities: v.caps}, nil
}

func planWithCaps(caps ...string) string {
	q, _ := json.Marshal(caps)
	return `{"subject":"t","steps":[{"query":"do it","depends_on":[],"required_capabilities":` +
		string(q) + `}]}`
}

// REGRESSION (2026-07-28): the planner emitted `["file_write"]` when no agent declared
// it and it was absent from the rendered vocabulary. With L1 enforcing, an undeclared
// tag matches nothing, filters every candidate, and the step dies with "no candidates
// found". A hallucinated tag must not be able to hard-fail a step.
func TestPlanner_DropsInventedCapabilityTag(t *testing.T) {
	gen := &mockGenerator{response: planWithCaps("file_read", "file_write")}
	p := NewPlanner(gen, &vocabProvider{caps: []string{"file_read", "general_purpose"}}, nil)
	p.SetCapabilityContract(true)

	plan, err := p.GetExecutionPlan(context.Background(), "write a file")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := plan.Steps[0].RequiredCapabilities
	if len(got) != 1 || got[0] != "file_read" {
		t.Fatalf("invented tag must be dropped and declared ones kept, got %v", got)
	}
}

// The guard must not fire on an absence of knowledge: an empty vocabulary means no
// manifest declared anything, not that every tag is invented.
func TestPlanner_EmptyVocabularyDoesNotStripCapabilities(t *testing.T) {
	gen := &mockGenerator{response: planWithCaps("file_read")}
	p := NewPlanner(gen, &mockAgentProvider{}, nil) // no agents, no manifests
	p.SetCapabilityContract(true)

	plan, err := p.GetExecutionPlan(context.Background(), "read a file")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := plan.Steps[0].RequiredCapabilities; len(got) != 1 {
		t.Fatalf("empty vocabulary must not strip requirements, got %v", got)
	}
}

// A step whose every tag is invented ends up unconstrained — the pre-ROUTE-03 path —
// rather than dead. That fallback IS the decision, so it is pinned.
func TestPlanner_AllInventedTagsLeaveStepUnconstrained(t *testing.T) {
	gen := &mockGenerator{response: planWithCaps("file_write", "telepathy")}
	p := NewPlanner(gen, &vocabProvider{caps: []string{"file_read"}}, nil)
	p.SetCapabilityContract(true)

	plan, err := p.GetExecutionPlan(context.Background(), "do a thing")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := plan.Steps[0].RequiredCapabilities; len(got) != 0 {
		t.Fatalf("all-invented requirements must leave the step unconstrained, got %v", got)
	}
}

// stubWorkspace returns a fixed enrichment, so a test can control exactly which
// routines the planner was shown.
type stubWorkspace struct{ enrichment domain.LTMEnrichment }

func (s *stubWorkspace) PrimeForPlanning(context.Context, string) (domain.LTMEnrichment, error) {
	return s.enrichment, nil
}

func (s *stubWorkspace) PrimeForExecution(context.Context, *domain.ExecutionPlan, map[string]string) (map[string]string, error) {
	return nil, nil
}

func (s *stubWorkspace) PrimeForStep(context.Context, string, []domain.ContextRef, []domain.SearchResult, float64, int) ([]domain.ContextRef, error) {
	return nil, nil
}

// TestPlanner_RecordsFollowedProcedures is the first hop of the ADR-0094 D8 loop.
//
// A routine the planner was SHOWN must be recorded on the plan, or the outcome can
// never feed back to it. Before this, PlanRecord.FollowedProcedures was read by the
// memory agent and written by nobody, so no routine's confidence ever moved.
func TestPlanner_RecordsFollowedProcedures(t *testing.T) {
	gen := &mockGenerator{response: minimalPlanJSON()}
	p := NewPlanner(gen, &mockAgentProvider{}, nil)
	p.WorkspaceStage = &stubWorkspace{enrichment: domain.LTMEnrichment{
		Procedures: []domain.Procedure{
			{ID: "proc-a", Trigger: "ship the thing", Confidence: 0.8, SampleCount: 4},
			{ID: "proc-b", Trigger: "ship the other thing", Confidence: 0.6, SampleCount: 3},
			{ID: "", Trigger: "malformed"}, // no id — nothing to feed back to
		},
	}}

	plan, err := p.GetExecutionPlan(context.Background(), "ship the thing")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.FollowedProcedures) != 2 {
		t.Fatalf("expected both identified routines recorded, got %v", plan.FollowedProcedures)
	}
	// Every routine SHOWN counts, not only ones provably used: a routine that was
	// present and did not help is exactly the evidence that should lower its
	// confidence. Requiring proof of use would record successes only.
	if plan.FollowedProcedures[0] != "proc-a" || plan.FollowedProcedures[1] != "proc-b" {
		t.Errorf("provenance must be the routine ids in order, got %v", plan.FollowedProcedures)
	}
}

// The provenance must survive the plan freeze, or a replanned plan silently stops
// feeding back — ExecutionPlan.Clone is field-by-field and omissions are invisible.
func TestPlanClone_PreservesFollowedProcedures(t *testing.T) {
	original := &domain.ExecutionPlan{
		Subject:            "s",
		Steps:              []domain.Step{{Query: "do it"}},
		FollowedProcedures: []string{"proc-a", "proc-b"},
	}
	cloned := original.Clone()
	if len(cloned.FollowedProcedures) != 2 {
		t.Fatalf("Clone dropped the routine provenance: %v", cloned.FollowedProcedures)
	}
	// A deep copy, so a replan cannot mutate the original's provenance.
	cloned.FollowedProcedures[0] = "mutated"
	if original.FollowedProcedures[0] != "proc-a" {
		t.Error("Clone aliased the provenance slice instead of copying it")
	}
}
