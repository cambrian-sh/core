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
	if DefaultConfig().Execution.Workers.Composed {
		t.Fatal("DefaultConfig turns composed workers on")
	}
}
