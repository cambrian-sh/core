package app

import (
	"log/slog"
	"strings"

	"github.com/cambrian-sh/core/internal/config"
)

// undeclaredPlannerModel reports whether execution.retrieval.agentic_planner_model
// names a generator that llm_provider.generators does not declare. The key takes a
// generator id ("llm:<id>"), not a role, and its shipped default is "llm:deepseek",
// which a fresh install may not declare — so turning agentic retrieval on could
// silently plan on a model that does not exist, or (when it does exist) keep planning
// on it after every role was switched elsewhere
// (T-20261102-agentic-planner-model-names-a-generator-id). "" means the gateway
// default and is never reported.
func undeclaredPlannerModel(model string, gens []config.GeneratorConfig) bool {
	id := strings.TrimPrefix(strings.TrimSpace(model), "llm:")
	if id == "" {
		return false
	}
	for _, g := range gens {
		if g.ID == id {
			return false
		}
	}
	return true
}

// warnUndeclaredPlannerModel logs the boot WARN; it changes nothing about which model
// the planner asks for (making the key follow a role is an owner decision).
func warnUndeclaredPlannerModel(model string, gens []config.GeneratorConfig) {
	if !undeclaredPlannerModel(model, gens) {
		return
	}
	slog.Warn("AGENTIC_RETRIEVAL_SPEC: execution.retrieval.agentic_planner_model names a generator that "+
		"llm_provider.generators does not declare; set it to a declared id, or \"\" for the gateway default",
		"agentic_planner_model", model, "ticket", "T-20261102-agentic-planner-model-names-a-generator-id")
}
