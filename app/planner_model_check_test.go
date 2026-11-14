package app

import (
	"testing"

	"github.com/cambrian-sh/core/internal/config"
)

func TestUndeclaredPlannerModel(t *testing.T) {
	t.Parallel()
	gens := []config.GeneratorConfig{{ID: "gemini-flash-lite"}, {ID: "mimo"}}
	for model, want := range map[string]bool{
		"":                       false, // gateway default
		"llm:mimo":               false,
		"mimo":                   false,
		"llm:deepseek":           true, // the shipped default, not declared here
		" llm:gemini-flash-lite": false,
	} {
		if got := undeclaredPlannerModel(model, gens); got != want {
			t.Errorf("undeclaredPlannerModel(%q) = %v, want %v", model, got, want)
		}
	}
}
