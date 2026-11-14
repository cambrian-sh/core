package app

import (
	"log/slog"
	"strings"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// roleGeneratorSource is the slice of *llm.Provider the retrieval planner needs:
// the role table's generator for a purpose, with the RoleFallbacks alias applied.
type roleGeneratorSource interface {
	RoleGenerator(purpose domain.Purpose) string
}

// retrievalPlannerRole resolves the agentic retrieval planner's generator through
// the role table on every call — the `retrieval_planner` purpose, falling back to
// the `memory` role (llm.RoleFallbacks) — so a console role switch moves the
// planner on its next call (owner decision 2026-11-14, third round;
// T-20261102-agentic-planner-model-names-a-generator-id). An explicit
// execution.retrieval.agentic_planner_model overrides it in the dispatcher.
func retrievalPlannerRole(p roleGeneratorSource) func() string {
	if p == nil {
		return nil
	}
	return func() string { return p.RoleGenerator(domain.PurposeRetrievalPlanner) }
}

// undeclaredPlannerModel reports whether an EXPLICIT
// execution.retrieval.agentic_planner_model names a generator that
// llm_provider.generators does not declare. "" (the default: the planner follows
// the retrieval_planner → memory role) is never reported
// (T-20261102-agentic-planner-model-names-a-generator-id).
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
// the planner asks for (an unknown id is skipped by the gateway's chain walk).
func warnUndeclaredPlannerModel(model string, gens []config.GeneratorConfig) {
	if !undeclaredPlannerModel(model, gens) {
		return
	}
	slog.Warn("AGENTIC_RETRIEVAL_SPEC: execution.retrieval.agentic_planner_model names a generator that "+
		"llm_provider.generators does not declare; set it to a declared id, or \"\" to follow the "+
		"retrieval_planner role (falling back to memory)",
		"agentic_planner_model", model, "ticket", "T-20261102-agentic-planner-model-names-a-generator-id")
}
