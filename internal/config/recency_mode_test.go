package config

import (
	"strings"
	"testing"
)

// T-20261102-stage-a-recency-is-min-max-over-the-candidate: the decay recency mode
// ships OFF (min-max, the measured default), its keys live in the Retrieval block and
// reach the fields from env, a bad mode or a non-positive half-life is refused, and
// the retrieval fingerprint is unchanged unless the mode is switched on.
func TestRecencyMode_DefaultsEnvValidationFingerprint(t *testing.T) {
	d := DefaultConfig().Execution
	if d.Retrieval.BlendRecencyMode != "minmax" || d.Retrieval.BlendRecencyHalfLifeHours != 2160 {
		t.Fatalf("defaults: mode %q half-life %v, want minmax / 2160", d.Retrieval.BlendRecencyMode, d.Retrieval.BlendRecencyHalfLifeHours)
	}

	dir := baseBundle(t)
	t.Setenv("CAMBRIAN_EXECUTION__RETRIEVAL__BLEND_RECENCY_MODE", "decay")
	t.Setenv("CAMBRIAN_EXECUTION__RETRIEVAL__BLEND_RECENCY_HALF_LIFE_HOURS", "8760")
	cfg, _ := loadFrom(t, dir, nil)
	if cfg.Execution.Retrieval.BlendRecencyMode != "decay" || cfg.Execution.Retrieval.BlendRecencyHalfLifeHours != 8760 {
		t.Fatalf("env did not reach the fields: %q %v", cfg.Execution.Retrieval.BlendRecencyMode, cfg.Execution.Retrieval.BlendRecencyHalfLifeHours)
	}

	bad := DefaultConfig().Execution
	bad.Retrieval.BlendRecencyMode = "linear"
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "blend_recency_mode") {
		t.Fatalf("an unknown mode must be refused, got %v", err)
	}
	bad = DefaultConfig().Execution
	bad.Retrieval.BlendRecencyMode = "decay"
	bad.Retrieval.BlendRecencyHalfLifeHours = 0
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "half_life") {
		t.Fatalf("decay with a zero half-life must be refused, got %v", err)
	}

	base := DefaultConfig().Execution
	same := DefaultConfig().Execution
	same.Retrieval.BlendRecencyHalfLifeHours = 1 // read only in decay mode
	if base.RetrievalFingerprint("e") != same.RetrievalFingerprint("e") {
		t.Fatal("in minmax mode the half-life must not change the fingerprint")
	}
	on := DefaultConfig().Execution
	on.Retrieval.BlendRecencyMode = "decay"
	if on.RetrievalFingerprint("e") == base.RetrievalFingerprint("e") {
		t.Fatal("decay mode changes ranking, so it must change the fingerprint")
	}
}
