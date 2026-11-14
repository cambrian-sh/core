package app

import (
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

func TestUndeclaredPlannerModel(t *testing.T) {
	t.Parallel()
	gens := []config.GeneratorConfig{{ID: "gemini-flash-lite"}, {ID: "mimo"}}
	for model, want := range map[string]bool{
		"":                       false, // follows the retrieval_planner role
		"llm:mimo":               false,
		"mimo":                   false,
		"llm:deepseek":           true, // an explicit id not declared here
		" llm:gemini-flash-lite": false,
	} {
		if got := undeclaredPlannerModel(model, gens); got != want {
			t.Errorf("undeclaredPlannerModel(%q) = %v, want %v", model, got, want)
		}
	}
}

type fakeRoleSource map[domain.Purpose]string

func (f fakeRoleSource) RoleGenerator(p domain.Purpose) string { return f[p] }

// The planner's role resolver asks for the retrieval_planner purpose (the Provider
// applies the memory fallback), and a nil provider leaves the gateway default.
func TestRetrievalPlannerRole(t *testing.T) {
	t.Parallel()
	if retrievalPlannerRole(nil) != nil {
		t.Fatal("a nil provider must yield no role resolver")
	}
	fn := retrievalPlannerRole(fakeRoleSource{domain.PurposeRetrievalPlanner: "mem"})
	if got := fn(); got != "mem" {
		t.Fatalf("retrievalPlannerRole() = %q, want mem", got)
	}
}
