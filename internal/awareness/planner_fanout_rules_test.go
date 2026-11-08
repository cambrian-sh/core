package awareness

import (
	"strings"
	"testing"
)

// T-20261107-planner-prompt-does-not-steer-fan-out-no-rule: a side-effecting
// fan-out expands only over a STRUCTURED source list (domain.ResolveFanOut), so
// every plan producer — both planner arms and both replan arms — must ask for a
// source step that returns its items as a JSON array, a parametric query that
// describes one item, and one list/bulk call over a per-record fan-out when the
// list already answers. Checked on the bytes each producer sends.
func TestPlanProducersCarryTheFanOutSourceRules(t *testing.T) {
	bodies := map[string]string{
		"planner":     plannerPrompts.System.Body,
		"planner_cap": plannerCapPrompts.System.Body,
		"replan":      replanPrompts.System.Body,
		"replan_cap":  replanCapPrompts.System.Body,
	}
	rules := []string{
		"use one step that calls it instead of a per-record fan-out",
		"A parametric step's query describes one item and contains {item}",
		"returns its items as a JSON array of strings",
		"return their names as a JSON array",
	}
	for name, body := range bodies {
		for _, r := range rules {
			if !strings.Contains(body, r) {
				t.Errorf("%s lacks the fan-out rule %q", name, r)
			}
		}
	}
}
