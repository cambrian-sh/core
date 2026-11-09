package network

import (
	"testing"
	"time"

	"github.com/cambrian-sh/core/internal/config"
)

// ADR-0145 D9: with the shipped vocabulary only `read` is read-only; every term
// that writes, moves, deletes, executes, reaches out, sends, spends or administers
// keeps a step out of the step cache.
func TestStepCacheVocabulary_OnlyReadIsReadOnly(t *testing.T) {
	v := stepCacheVocabulary(config.CapabilityConfig{EffectVocabulary: config.DefaultEffectVocabulary()})
	if v == nil {
		t.Fatal("default vocabulary did not build")
	}
	for _, name := range v.Names() {
		if got, want := v.ReadOnly(name), name == "read"; got != want {
			t.Errorf("ReadOnly(%q) = %v, want %v", name, got, want)
		}
	}
	if v.ReadOnly("teleport") {
		t.Error("an unknown effect must not be read-only")
	}
}

func TestStepCacheDefaultTTL(t *testing.T) {
	if got := stepCacheDefaultTTL(config.PlanConfig{}); got != time.Hour {
		t.Errorf("unset: %v, want 1h", got)
	}
	if got := stepCacheDefaultTTL(config.PlanConfig{StepCacheDefaultTTLMinutes: 5}); got != 5*time.Minute {
		t.Errorf("5: %v, want 5m", got)
	}
	if got := config.DefaultConfig().Execution.Plan.StepCacheDefaultTTLMinutes; got != 60 {
		t.Errorf("shipped default = %d, want 60", got)
	}
}
