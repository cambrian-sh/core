package awareness

import (
	"context"
	"strings"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// staticProvider is a test-local PolicyProvider stub.
type staticProvider struct {
	known map[string]bool
}

func (s *staticProvider) GetPolicy(name string) (domain.HippocampusPolicy, bool) {
	_, ok := s.known[name]
	return domain.HippocampusPolicy{}, ok
}
func (s *staticProvider) DefaultPolicy() domain.HippocampusPolicy { return domain.HippocampusPolicy{} }

// Cycle 1 — prompt contains the cache_policy instruction block.
func TestPlanner_Prompt_ContainsCachePolicyInstructions(t *testing.T) {
	gen := &mockGenerator{response: `{"steps":[{"query":"q","depends_on":[]}],"subject":"s"}`}
	planner := NewPlanner(gen, &mockAgentProvider{}, nil)

	_, _ = planner.GetExecutionPlan(context.Background(), "do something")

	if len(gen.capturedPrompts) == 0 {
		t.Fatal("no Generate call recorded")
	}
	prompt := gen.capturedPrompts[0]
	if !strings.Contains(prompt, `"cache_policy"`) {
		t.Errorf("expected prompt to mention cache_policy field; got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "codegen") {
		t.Errorf("expected prompt to list 'codegen' policy; got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "cognitive") {
		t.Errorf("expected prompt to list 'cognitive' policy; got:\n%s", prompt)
	}
}

// Cycle 2 — CachePolicy is populated when the LLM emits it.
func TestPlanner_EmitsCachePolicy_PopulatedOnPlan(t *testing.T) {
	gen := &mockGenerator{response: `{"steps":[{"query":"q","depends_on":[]}],"subject":"s","cache_policy":"cognitive"}`}
	planner := NewPlanner(gen, &mockAgentProvider{}, nil)

	plan, err := planner.GetExecutionPlan(context.Background(), "analyse the data")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.CachePolicy != "cognitive" {
		t.Errorf("CachePolicy: want %q got %q", "cognitive", plan.CachePolicy)
	}
}

// Cycle 3 — unknown policy name is normalised to "" when PolicyProvider is wired.
func TestPlanner_UnknownCachePolicy_NormalisedToEmpty(t *testing.T) {
	gen := &mockGenerator{response: `{"steps":[{"query":"q","depends_on":[]}],"subject":"s","cache_policy":"hallucinated"}`}
	planner := NewPlanner(gen, &mockAgentProvider{}, nil)
	planner.SetPolicyProvider(&staticProvider{known: map[string]bool{
		"codegen": true, "cognitive": true, "tool": true, "research": true, "default": true,
	}})

	plan, err := planner.GetExecutionPlan(context.Background(), "do something")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.CachePolicy != "" {
		t.Errorf("unknown cache_policy should be normalised to empty string, got %q", plan.CachePolicy)
	}
}

// Cycle 4 — valid policy name is preserved when PolicyProvider is wired.
func TestPlanner_ValidCachePolicy_Preserved(t *testing.T) {
	gen := &mockGenerator{response: `{"steps":[{"query":"q","depends_on":[]}],"subject":"s","cache_policy":"tool"}`}
	planner := NewPlanner(gen, &mockAgentProvider{}, nil)
	planner.SetPolicyProvider(&staticProvider{known: map[string]bool{
		"codegen": true, "cognitive": true, "tool": true, "research": true, "default": true,
	}})

	plan, err := planner.GetExecutionPlan(context.Background(), "read the file")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.CachePolicy != "tool" {
		t.Errorf("valid cache_policy should be preserved, got %q", plan.CachePolicy)
	}
}

// Cycle 5 — CachePolicy is copied by Clone().
func TestExecutionPlan_Clone_CopiesCachePolicy(t *testing.T) {
	original := &domain.ExecutionPlan{
		Subject:     "s",
		CachePolicy: "research",
		Steps:       []domain.Step{{Query: "q"}},
	}
	cloned := original.Clone()
	if cloned.CachePolicy != "research" {
		t.Errorf("Clone() did not copy CachePolicy: got %q", cloned.CachePolicy)
	}
}
