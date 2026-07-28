package domain

import "testing"

// ExecutionPlan.Clone copies Step field-by-field, so a field omitted there is
// dropped on every replan and plan freeze with no compiler error — the same
// silent-drop shape that let the EFE selector discard the capability contract.
// This test is the guard for the pin fields specifically.
func TestClone_CarriesAgentPin(t *testing.T) {
	plan := &ExecutionPlan{
		Subject: "s",
		Steps: []Step{{
			Query:          "do the thing",
			PreferredAgent: "terminal_agent",
			AgentPin:       PinHard,
		}},
	}

	got := plan.Clone()

	if got.Steps[0].PreferredAgent != "terminal_agent" {
		t.Errorf("Clone dropped PreferredAgent: %q", got.Steps[0].PreferredAgent)
	}
	if got.Steps[0].AgentPin != PinHard {
		t.Errorf("Clone dropped AgentPin: %q", got.Steps[0].AgentPin)
	}
}
