package config

import (
	"strings"
	"testing"
)

// ADR-0136 Phase 0: the ranking-v2 switches (D1, D3) ship OFF, their keys live in
// the Retrieval block and reach the fields from env, bad values are refused, and the
// retrieval fingerprint is unchanged unless a switch is turned on (the A1 pattern:
// every existing run fingerprint stays valid).
//
// D2 (max_per_document) was rejected by the owner on 2026-11-03 and removed. Its
// env var is still set below: a deployment that kept it must boot, with the key
// ignored like any other unknown key (the decoder tolerates unknown keys).
func TestRankingV2_DefaultsEnvValidationFingerprint(t *testing.T) {
	d := DefaultConfig().Execution.Retrieval
	if d.StageACalibrated || d.TemporalIntentEnabled {
		t.Fatalf("ranking v2 must default off: %+v", d)
	}
	if d.LexicalSaturationK != 1.5 || d.TemporalFitWeight != 0.2 {
		t.Fatalf("tunable defaults: k %v fit %v, want 1.5 / 0.2", d.LexicalSaturationK, d.TemporalFitWeight)
	}

	dir := baseBundle(t)
	t.Setenv("CAMBRIAN_EXECUTION__RETRIEVAL__STAGE_A_CALIBRATED", "true")
	t.Setenv("CAMBRIAN_EXECUTION__RETRIEVAL__LEXICAL_SATURATION_K", "0.25")
	t.Setenv("CAMBRIAN_EXECUTION__RETRIEVAL__MAX_PER_DOCUMENT", "8") // removed D2 key: must be ignored, not fatal
	t.Setenv("CAMBRIAN_EXECUTION__RETRIEVAL__TEMPORAL_INTENT_ENABLED", "true")
	t.Setenv("CAMBRIAN_EXECUTION__RETRIEVAL__TEMPORAL_FIT_WEIGHT", "0.3")
	cfg, _ := loadFrom(t, dir, nil)
	r := cfg.Execution.Retrieval
	if !r.StageACalibrated || r.LexicalSaturationK != 0.25 || !r.TemporalIntentEnabled || r.TemporalFitWeight != 0.3 {
		t.Fatalf("env did not reach the fields: %+v", r)
	}
	if err := cfg.Execution.Validate(); err != nil {
		t.Fatalf("a config still carrying the removed max_per_document key must validate: %v", err)
	}

	for name, mut := range map[string]func(*ExecutionConfig){
		"lexical_saturation_k": func(c *ExecutionConfig) {
			c.Retrieval.StageACalibrated = true
			c.Retrieval.LexicalSaturationK = 0
		},
		"temporal_fit_weight": func(c *ExecutionConfig) { c.Retrieval.TemporalFitWeight = -0.1 },
	} {
		bad := DefaultConfig().Execution
		mut(&bad)
		if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: a bad value must be refused, got %v", name, err)
		}
	}

	base := DefaultConfig().Execution
	same := DefaultConfig().Execution
	same.Retrieval.LexicalSaturationK = 9 // read only when stage_a_calibrated
	same.Retrieval.TemporalFitWeight = 9  // read only when temporal_intent_enabled
	if base.RetrievalFingerprint("e") != same.RetrievalFingerprint("e") {
		t.Fatal("a switch's tunables must not change the fingerprint while the switch is off")
	}
	for name, mut := range map[string]func(*ExecutionConfig){
		"stage_a_calibrated":      func(c *ExecutionConfig) { c.Retrieval.StageACalibrated = true },
		"temporal_intent_enabled": func(c *ExecutionConfig) { c.Retrieval.TemporalIntentEnabled = true },
	} {
		on := DefaultConfig().Execution
		mut(&on)
		if on.RetrievalFingerprint("e") == base.RetrievalFingerprint("e") {
			t.Errorf("%s changes ranking, so it must change the fingerprint", name)
		}
	}
}
