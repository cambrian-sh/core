package awareness

import (
	"context"
	"testing"
)

// planner_hermes_test.go — Thought-block integration tests for Planner.GetExecutionPlan.
//
// mockGenerator and mockAgentProvider are defined in planner_test.go and shared
// across the package.

func TestPlanner_WithThoughts_ExtractsPlan(t *testing.T) {
	resp := `<thought>I need to use the music tool.</thought>{"steps":[{"required_tools":["music-player"],"query":"play KMFDM","depends_on":[]}],"subject":"Music"}`

	gen := &mockGenerator{response: resp}
	planner := NewPlanner(gen, &mockAgentProvider{}, nil)

	plan, err := planner.GetExecutionPlan(context.Background(), "play some KMFDM")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan == nil {
		t.Fatal("expected non-nil plan")
	}
	if plan.Subject != "Music" {
		t.Errorf("plan.Subject = %q, want %q", plan.Subject, "Music")
	}
}

func TestPlanner_WithoutThoughts_UnchangedBehaviour(t *testing.T) {
	resp := `{"steps":[{"required_tools":[],"query":"do something","depends_on":[]}],"subject":"Test"}`

	gen := &mockGenerator{response: resp}
	planner := NewPlanner(gen, &mockAgentProvider{}, nil)

	plan, err := planner.GetExecutionPlan(context.Background(), "do something")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan == nil {
		t.Fatal("expected non-nil plan")
	}
	if plan.Subject != "Test" {
		t.Errorf("plan.Subject = %q, want %q", plan.Subject, "Test")
	}
}

func TestPlanner_MultipleThoughts_AllExtracted(t *testing.T) {
	resp := `<thought>First: identify the domain.</thought><thought>Second: pick the right tool.</thought>{"steps":[{"required_tools":["search"],"query":"search for cats","depends_on":[]}],"subject":"Cats"}`

	gen := &mockGenerator{response: resp}
	planner := NewPlanner(gen, &mockAgentProvider{}, nil)

	plan, err := planner.GetExecutionPlan(context.Background(), "find cats")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan == nil {
		t.Fatal("expected non-nil plan")
	}
	if plan.Subject != "Cats" {
		t.Errorf("plan.Subject = %q, want %q", plan.Subject, "Cats")
	}
}

func TestPlanner_ThoughtsOnly_NoJSON_ReturnsError(t *testing.T) {
	resp := `<thought>some reasoning</thought>`

	gen := &mockGenerator{response: resp}
	planner := NewPlanner(gen, &mockAgentProvider{}, nil)

	plan, err := planner.GetExecutionPlan(context.Background(), "something")
	if err == nil {
		t.Fatalf("expected an error when no JSON is present, got nil (plan: %+v)", plan)
	}
}
