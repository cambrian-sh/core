package config

import "testing"

// ADR-0050 D1, the single-agent mode: default OFF, and the two keys reach the
// Routing block from their CAMBRIAN_* env vars — the form the use-case suite's
// baseline arm sets them in (--kernel-env;
// T-20261112-baseline-arm-run-the-use-case-journeys-through).
func TestLoadConfig_BypassSelectionKeys(t *testing.T) {
	if DefaultConfig().Execution.Routing.BypassSelection {
		t.Fatal("execution.routing.bypass_selection must default OFF")
	}
	dir := baseBundle(t)
	t.Setenv("CAMBRIAN_EXECUTION__ROUTING__BYPASS_SELECTION", "true")
	t.Setenv("CAMBRIAN_EXECUTION__ROUTING__SINGLE_AGENT_ID", "uc-personal-assistant")
	cfg, _ := loadFrom(t, dir, nil)
	if !cfg.Execution.Routing.BypassSelection {
		t.Fatal("CAMBRIAN_EXECUTION__ROUTING__BYPASS_SELECTION did not reach the field")
	}
	if got := cfg.Execution.Routing.SingleAgentID; got != "uc-personal-assistant" {
		t.Fatalf("single_agent_id = %q, want uc-personal-assistant", got)
	}
}

// The baseline arm's whole kernel env (run-baseline.sh: the single-agent mode
// with every ADR-0145 layer explicitly off) loads, and the layer keys read off.
func TestLoadConfig_SingleAgentBaselineArmEnv(t *testing.T) {
	dir := baseBundle(t)
	for k, v := range map[string]string{
		"CAMBRIAN_EXECUTION__ROUTING__BYPASS_SELECTION":      "true",
		"CAMBRIAN_EXECUTION__ROUTING__SINGLE_AGENT_ID":       "uc-personal-assistant",
		"CAMBRIAN_EXECUTION__LEDGER__ENABLED":                "false",
		"CAMBRIAN_EXECUTION__PLAN__CHECKLIST_ENABLED":        "false",
		"CAMBRIAN_EXECUTION__PLAN__PLAN_VALIDATOR":           "false",
		"CAMBRIAN_EXECUTION__PLAN__STAGED_PLANNER":           "false",
		"CAMBRIAN_EXECUTION__PLAN__ASK_BACK_ENABLED":         "false",
		"CAMBRIAN_EXECUTION__PLAN__PLAN_MAX_DURATION_MS":     "7200000",
		"CAMBRIAN_EXECUTION__PLAN__PLAN_IDLE_TIMEOUT_MS":     "1800000",
		"CAMBRIAN_EXECUTION__TOOLS__VALIDATE_ARGS":           "false",
		"CAMBRIAN_EXECUTION__TOOLS__IDEMPOTENT_WRITES":       "false",
		"CAMBRIAN_EXECUTION__TOOLS__CALC_TOOLS":              "false",
		"CAMBRIAN_EXECUTION__WORKERS__COMPOSED":              "false",
		"CAMBRIAN_EXECUTION__WORKERS__EVIDENCE_REPROMPT":     "false",
		"CAMBRIAN_EXECUTION__CHAT__TRANSCRIPT_BUDGET_TOKENS": "0",
		"CAMBRIAN_EXECUTION__CHAT__TURN_INDEX":               "false",
		"CAMBRIAN_EXECUTION__CHAT__GROUNDED_REPLY":           "false",
		"CAMBRIAN_EXECUTION__CHAT__REPLY_CHECK":              "off",
		"CAMBRIAN_EXECUTION__CHAT__DELEGATION_RULE":          "false",
		"CAMBRIAN_EXECUTION__CHAT__LEDGER_TOOLS":             "false",
		"CAMBRIAN_EXECUTION__CHAT__ASK_BACK":                 "false",
	} {
		t.Setenv(k, v)
	}
	cfg, _ := loadFrom(t, dir, nil)
	e := cfg.Execution
	if !e.Routing.BypassSelection || e.Routing.SingleAgentID != "uc-personal-assistant" {
		t.Fatalf("routing = %+v, want the single-agent mode on uc-personal-assistant", e.Routing)
	}
	if e.Ledger.Enabled || e.Plan.ChecklistEnabled || e.Plan.StagedPlanner || e.Plan.PlanValidator ||
		e.Workers.Composed || e.Tools.CalcTools || e.Chat.GroundedReply || e.Chat.DelegationRule {
		t.Fatal("an ADR-0145 layer read on in the baseline arm's env")
	}
}
