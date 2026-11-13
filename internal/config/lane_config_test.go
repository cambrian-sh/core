package config

import (
	"testing"
)

// ADR-0150 D10: execution.lane defaults to the single-agent lane with the ADR's
// runaway caps; the zero value (a hand-built config) is the planner lane.
func TestLaneConfig_Defaults(t *testing.T) {
	l := DefaultConfig().Execution.Lane
	if !l.IsSingleAgent() || l.Mode() != LaneSingleAgent {
		t.Fatalf("default lane = %q, want single_agent", l.Default)
	}
	if l.ToolRounds() != 500 || l.WallSeconds() != 3600 || l.SameResultRepeats() != 4 {
		t.Fatalf("caps = %d rounds, %d s, %d repeats; want 500, 3600, 4", l.ToolRounds(), l.WallSeconds(), l.SameResultRepeats())
	}
	if l.SingleAgent.AgentID != "" {
		t.Fatalf("agent_id default = %q, want \"\" (the composed runtime)", l.SingleAgent.AgentID)
	}
	if (LaneConfig{}).IsSingleAgent() {
		t.Fatal("the zero LaneConfig must be the planner lane")
	}
	c := DefaultConfig().Execution.Chat
	if c.KeepUserMessagesTokens != 20000 {
		t.Fatalf("chat.keep_user_messages_tokens = %d, want 20000", c.KeepUserMessagesTokens)
	}
	if DefaultConfig().Execution.Content.KeepRecentToolResults != 0 {
		t.Fatal("content.keep_recent_tool_results must default off")
	}
}

func TestLaneConfig_Validation(t *testing.T) {
	bad := DefaultConfig().Execution
	bad.Lane.Default = "router"
	if errs := bad.Lane.validate(); len(errs) == 0 {
		t.Fatal("an unknown lane must be refused")
	}
	neg := DefaultConfig().Execution
	neg.Lane.SingleAgent.MaxToolRounds = -1
	if errs := neg.Lane.validate(); len(errs) == 0 {
		t.Fatal("a negative cap must be refused")
	}
	for _, mode := range []string{"single_agent", "planner", "PLANNER"} {
		ok := DefaultConfig().Execution
		ok.Lane.Default = mode
		if errs := ok.Lane.validate(); len(errs) != 0 {
			t.Fatalf("lane %q refused: %v", mode, errs)
		}
	}
}

// Both values of the switch load from a file and from the environment.
func TestLoadConfig_LaneSwitch(t *testing.T) {
	dir := baseBundle(t)
	writeCfg(t, dir, "tuning.json", `{"execution": {"lane": {"default": "planner"}}}`)
	cfg, _ := loadFrom(t, dir, nil)
	if cfg.Execution.Lane.IsSingleAgent() {
		t.Fatal("tuning.json lane.default planner did not load")
	}
	t.Setenv("CAMBRIAN_EXECUTION__LANE__DEFAULT", "single_agent")
	cfg, _ = loadFrom(t, dir, nil)
	if !cfg.Execution.Lane.IsSingleAgent() {
		t.Fatal("CAMBRIAN_EXECUTION__LANE__DEFAULT did not override the file")
	}
}

// The ADR-0050 D1 keys of execution.routing still load — as execution.lane — in
// every layer: a file (nested and v1 flat), the durable store and the environment.
func TestLoadConfig_LegacyRoutingKeysMigrateToLane(t *testing.T) {
	t.Run("file nested", func(t *testing.T) {
		dir := baseBundle(t)
		writeCfg(t, dir, "tuning.json", `{"execution": {"lane": {"default": "planner"}}}`)
		writeCfg(t, dir, "tuning.local.json", `{"execution": {"routing": {"bypass_selection": true, "single_agent_id": "uc-personal-assistant"}}}`)
		cfg, _ := loadFrom(t, dir, nil)
		if !cfg.Execution.Lane.IsSingleAgent() || cfg.Execution.Lane.SingleAgent.AgentID != "uc-personal-assistant" {
			t.Fatalf("lane = %+v", cfg.Execution.Lane)
		}
	})
	t.Run("file v1 flat", func(t *testing.T) {
		dir := baseBundle(t)
		writeCfg(t, dir, "tuning.json", `{"execution": {"lane": {"default": "planner"}}}`)
		writeCfg(t, dir, "tuning.local.json", `{"execution": {"bypass_selection": true, "single_agent_id": "a1"}}`)
		cfg, _ := loadFrom(t, dir, nil)
		if !cfg.Execution.Lane.IsSingleAgent() || cfg.Execution.Lane.SingleAgent.AgentID != "a1" {
			t.Fatalf("lane = %+v", cfg.Execution.Lane)
		}
	})
	t.Run("new key in the same layer wins", func(t *testing.T) {
		dir := baseBundle(t)
		writeCfg(t, dir, "tuning.json", `{"execution": {"routing": {"bypass_selection": true, "single_agent_id": "old"}, "lane": {"default": "planner", "single_agent": {"agent_id": "new"}}}}`)
		cfg, _ := loadFrom(t, dir, nil)
		if cfg.Execution.Lane.IsSingleAgent() || cfg.Execution.Lane.SingleAgent.AgentID != "new" {
			t.Fatalf("lane = %+v", cfg.Execution.Lane)
		}
	})
	t.Run("bypass_selection false is dropped", func(t *testing.T) {
		dir := baseBundle(t)
		writeCfg(t, dir, "tuning.json", `{"execution": {"routing": {"bypass_selection": false}}}`)
		cfg, _ := loadFrom(t, dir, nil)
		if !cfg.Execution.Lane.IsSingleAgent() {
			t.Fatal("bypass_selection false must not select the planner lane")
		}
	})
	t.Run("store", func(t *testing.T) {
		dir := baseBundle(t)
		writeCfg(t, dir, "tuning.json", `{"execution": {"lane": {"default": "planner"}}}`)
		cfg, _ := loadFrom(t, dir, mapStore{
			"execution.routing.bypass_selection": true,
			"execution.single_agent_id":          "s1",
		})
		if !cfg.Execution.Lane.IsSingleAgent() || cfg.Execution.Lane.SingleAgent.AgentID != "s1" {
			t.Fatalf("lane = %+v", cfg.Execution.Lane)
		}
	})
	t.Run("environment", func(t *testing.T) {
		dir := baseBundle(t)
		writeCfg(t, dir, "tuning.json", `{"execution": {"lane": {"default": "planner"}}}`)
		t.Setenv("CAMBRIAN_EXECUTION__ROUTING__BYPASS_SELECTION", "true")
		t.Setenv("CAMBRIAN_EXECUTION__ROUTING__SINGLE_AGENT_ID", "uc-personal-assistant")
		cfg, _ := loadFrom(t, dir, nil)
		if !cfg.Execution.Lane.IsSingleAgent() || cfg.Execution.Lane.SingleAgent.AgentID != "uc-personal-assistant" {
			t.Fatalf("lane = %+v", cfg.Execution.Lane)
		}
	})
	t.Run("environment new variable wins", func(t *testing.T) {
		dir := baseBundle(t)
		t.Setenv("CAMBRIAN_EXECUTION__ROUTING__BYPASS_SELECTION", "true")
		t.Setenv("CAMBRIAN_EXECUTION__LANE__DEFAULT", "planner")
		cfg, _ := loadFrom(t, dir, nil)
		if cfg.Execution.Lane.IsSingleAgent() {
			t.Fatal("CAMBRIAN_EXECUTION__LANE__DEFAULT must win over the old variable")
		}
	})
}

// cmd/config-migrate rewrites the old keys to the new place.
func TestMigrateFileInPlace_LaneKeys(t *testing.T) {
	dir := t.TempDir()
	writeCfg(t, dir, "tuning.json", `{"execution": {"routing": {"bypass_selection": true, "single_agent_id": "x"}}}`)
	moved, err := MigrateFileInPlace(dir + "/tuning.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(moved) != 2 {
		t.Fatalf("moved = %v, want both routing keys", moved)
	}
	again, _ := MigrateFileInPlace(dir + "/tuning.json")
	if len(again) != 0 {
		t.Fatalf("second migrate moved %v; must be idempotent", again)
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
	if !e.Lane.IsSingleAgent() || e.Lane.SingleAgent.AgentID != "uc-personal-assistant" {
		t.Fatalf("lane = %+v, want the single-agent lane on uc-personal-assistant", e.Lane)
	}
	if e.Ledger.Enabled || e.Plan.ChecklistEnabled || e.Plan.StagedPlanner || e.Plan.PlanValidator ||
		e.Workers.Composed || e.Tools.CalcTools || e.Chat.GroundedReply || e.Chat.DelegationRule {
		t.Fatal("an ADR-0145 layer read on in the baseline arm's env")
	}
}
