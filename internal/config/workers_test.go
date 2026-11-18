package config

import (
	"strings"
	"testing"
)

func TestWorkersConfig_DefaultsAndValidation(t *testing.T) {
	var w WorkersConfig
	if w.Composed || w.EvidenceReprompt {
		t.Fatal("ADR-0145 D12/D3 flags must default off")
	}
	if w.RuntimeID() != DefaultComposedRuntimeAgentID || w.SkillCap() != DefaultComposedMaxSkills {
		t.Fatalf("defaults: %q %d", w.RuntimeID(), w.SkillCap())
	}
	if errs := (WorkersConfig{Composed: true}).validate(false); len(errs) != 1 || !strings.Contains(errs[0], "ledger") {
		t.Fatalf("composed without the ledger must be refused: %v", errs)
	}
	if errs := (WorkersConfig{Composed: true}).validate(true); len(errs) != 0 {
		t.Fatalf("composed with the ledger: %v", errs)
	}
	if errs := (WorkersConfig{RuntimeAgentID: "Bad Id", MaxSkills: -1}).validate(true); len(errs) != 2 {
		t.Fatalf("bad id and negative cap: %v", errs)
	}
	if errs := (WorkersConfig{MaxToolRounds: -1}).validate(true); len(errs) != 1 || !strings.Contains(errs[0], "max_tool_rounds") {
		t.Fatalf("negative tool budget: %v", errs)
	}
	if DefaultConfig().Execution.Workers.MaxToolRounds != 0 {
		t.Fatal("DefaultConfig must leave the composed runtime's default tool budget")
	}
	// ADR-0150 (owner 2026-11-13): DefaultConfig ships composed workers AND the
	// ledger they require on, and the pair validates.
	def := DefaultConfig().Execution
	if !def.Workers.Composed || !def.Ledger.Enabled {
		t.Fatalf("DefaultConfig: composed=%v ledger=%v, want both on", def.Workers.Composed, def.Ledger.Enabled)
	}
	if errs := def.Workers.validate(def.Ledger.Enabled); len(errs) != 0 {
		t.Fatalf("default workers block: %v", errs)
	}
}

// TestADR0150Defaults_LoadAndOptOut pins the owner's 2026-11-13 defaults through
// the full layered load (not just the struct), and that an operator's explicit
// false / 0 in a file still turns each one off — the defaults are true / non-zero,
// so a dropped "false" would silently re-enable them.
func TestADR0150Defaults_LoadAndOptOut(t *testing.T) {
	cfg, _ := loadFrom(t, baseBundle(t), nil)
	e := cfg.Execution
	if !e.Workers.Composed || !e.Ledger.Enabled || e.Chat.TranscriptBudgetTokens != 64000 {
		t.Fatalf("defaults: composed=%v ledger=%v transcript_budget_tokens=%d, want true/true/64000",
			e.Workers.Composed, e.Ledger.Enabled, e.Chat.TranscriptBudgetTokens)
	}

	off := baseBundle(t)
	writeCfg(t, off, "tuning.json", `{"schema_version":2,"execution":{"workers":{"composed":false},"ledger":{"enabled":false},"chat":{"transcript_budget_tokens":0}}}`)
	cfg, _ = loadFrom(t, off, nil)
	e = cfg.Execution
	if e.Workers.Composed || e.Ledger.Enabled || e.Chat.TranscriptBudgetTokens != 0 {
		t.Fatalf("explicit opt-out: composed=%v ledger=%v transcript_budget_tokens=%d, want false/false/0",
			e.Workers.Composed, e.Ledger.Enabled, e.Chat.TranscriptBudgetTokens)
	}
}

// ENG-2083: the third-party composed runtime's id derives from the runtime id,
// and a runtime id too long to carry the suffix is refused while composed.
func TestWorkersConfig_ThirdPartyRuntimeID(t *testing.T) {
	if got := (WorkersConfig{}).ThirdPartyRuntimeID(); got != "composed_worker_third_party" {
		t.Fatalf("default third-party runtime id = %q", got)
	}
	if got := (WorkersConfig{RuntimeAgentID: "rt"}).ThirdPartyRuntimeID(); got != "rt_third_party" {
		t.Fatalf("derived id = %q", got)
	}
	long := "a" + strings.Repeat("b", 55) // a valid 56-char id; with the suffix it is 68
	if errs := (WorkersConfig{Composed: true, RuntimeAgentID: long}).validate(true); len(errs) != 1 || !strings.Contains(errs[0], "third-party runtime") {
		t.Fatalf("a runtime id that cannot carry the suffix must be refused: %v", errs)
	}
	if errs := (WorkersConfig{RuntimeAgentID: long}).validate(true); len(errs) != 0 {
		t.Fatalf("composed off: no third-party runtime, nothing to refuse: %v", errs)
	}
}
