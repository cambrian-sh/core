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
