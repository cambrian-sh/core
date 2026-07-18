package awareness

import (
	"context"
	"strings"
	"testing"
)

// The planner prompt must advertise fan-out, and a plan that uses it must round-trip
// through parsing with the parametric fields intact (they unmarshal straight into
// domain.Step — a DTO that dropped them would silently downgrade fan-out to a literal step).
func TestPlanner_FanOut_PromptAdvertisesAndParses(t *testing.T) {
	gen := &mockGenerator{response: `{"steps":[
		{"query":"scan the folder","depends_on":[]},
		{"query":"write the file for {item}","depends_on":[0],"fan_out_over":0},
		{"query":"summarize","depends_on":[1]}
	],"subject":"docs"}`}
	planner := NewPlanner(gen, &mockAgentProvider{}, nil)

	plan, err := planner.GetExecutionPlan(context.Background(), "write the missing sections")
	if err != nil {
		t.Fatalf("GetExecutionPlan: %v", err)
	}

	// The prompt must teach the planner the feature exists.
	if len(gen.capturedPrompts) == 0 || !strings.Contains(gen.capturedPrompts[0], "fan_out_over") {
		t.Error("planner prompt does not advertise fan_out_over")
	}

	// The parametric step must survive parsing.
	if len(plan.Steps) != 3 {
		t.Fatalf("want 3 steps, got %d", len(plan.Steps))
	}
	s := plan.Steps[1]
	if s.FanOutOver == nil || *s.FanOutOver != 0 {
		t.Fatalf("fan_out_over not parsed into the step: %+v", s)
	}
	if s.FanOutVarName() != "item" {
		t.Errorf("default fan-out var should be 'item', got %q", s.FanOutVarName())
	}
}
