package config

import (
	"strings"
	"testing"
)

// execution.lane.context rejects negative values; 0 means "not set".
func TestLaneContext_Validate(t *testing.T) {
	if errs := (LaneContextConfig{}).validate(); len(errs) != 0 {
		t.Fatalf("zero block must be valid: %v", errs)
	}
	errs := (LaneContextConfig{ProtectTail: -1}).validate()
	if len(errs) != 1 || !strings.Contains(errs[0], "execution.lane.context") {
		t.Fatalf("negative protect_tail: %v", errs)
	}
}

// The lane block wins over execution.content for the shared keys, and an unset
// lane key leaves execution.content's value (ENG-595, owner decision 2026-11-14).
func TestEffectiveContent_LaneWins(t *testing.T) {
	e := ExecutionConfig{Content: ContentConfig{WorkerPromptBudgetTokens: 64000, KeepRecentToolResults: 6}}
	if c := e.EffectiveContent(); c.WorkerPromptBudget() != 64000 || c.KeepRecentToolResults != 6 {
		t.Fatalf("unset lane changed content: %+v", c)
	}
	e.Lane.Context = LaneContextConfig{BudgetTokens: 40000, KeepRecentToolResults: 2}
	if c := e.EffectiveContent(); c.WorkerPromptBudget() != 40000 || c.KeepRecentToolResults != 2 {
		t.Fatalf("lane did not win: %+v", c)
	}
	if !e.AgentContext().IsZero() && e.AgentContext().BudgetTokens != 40000 {
		t.Fatalf("AgentContext = %+v", e.AgentContext())
	}
}
