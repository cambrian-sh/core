package awareness

import (
	"context"
	"testing"
)

// The planner's reply contract (ADR-0139 C1): the schema asks for a reasoning
// field first, in place of the old free-form <thought> preamble and its scraper.

func TestPlanner_ReasoningFieldIsDecodedNotExecuted(t *testing.T) {
	t.Parallel()
	gen := &requestRecorder{reply: `{"reasoning":"I need the music tool.","subject":"Music","cache_policy":"tool","steps":[{"query":"play KMFDM","depends_on":[]}]}`}
	plan, err := NewPlanner(gen, &mockAgentProvider{}).GetExecutionPlan(context.Background(), "play some KMFDM")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.Subject != "Music" || len(plan.Steps) != 1 || plan.Steps[0].Query != "play KMFDM" {
		t.Fatalf("plan = %+v", plan)
	}
}

// A validated reply is decoded as is: nothing is scraped out of it.
func TestPlanner_ValidatedReplyIsNotScraped(t *testing.T) {
	t.Parallel()
	gen := &requestRecorder{reply: `<thought>x</thought>{"reasoning":"","subject":"s","cache_policy":"","steps":[]}`}
	if _, err := NewPlanner(gen, &mockAgentProvider{}).GetExecutionPlan(context.Background(), "x"); err == nil {
		t.Fatal("a reply that is not JSON must fail; the provider layer validates real replies")
	}
}

// A generator that ignored the schema may fence its JSON; nothing else is tolerated.
func TestPlanner_UncheckedReplyToleratesOnlyAFence(t *testing.T) {
	t.Parallel()
	const js = `{"subject":"Test","steps":[{"query":"do something","depends_on":[]}]}`
	for raw, ok := range map[string]bool{
		"```json\n" + js + "\n```":          true,
		js:                                  true,
		"<thought>reasoning</thought>" + js: false,
		"<thought>some reasoning</thought>": false,
	} {
		plan, err := NewPlanner(&mockGenerator{response: raw}, &mockAgentProvider{}).GetExecutionPlan(context.Background(), "do something")
		if (err == nil) != ok {
			t.Errorf("%q: err = %v, want ok=%v", raw, err, ok)
		}
		if ok && (plan == nil || plan.Subject != "Test") {
			t.Errorf("%q: plan = %+v", raw, plan)
		}
	}
}
