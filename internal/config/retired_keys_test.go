package config

import (
	"reflect"
	"strings"
	"testing"
)

// Retiring a config key must not break a deployment that still sets it: the config
// schema is a held-stable contract (root AGENTS.md §3.4, ADR-0057 D8). These tests
// pin that for the keys retired 2026-10-31 with the ADR-0029 episodic lane —
// `consolidation_threshold_doc_count`, `consolidation_threshold_bytes` and the
// "episodic" HippocampusPolicy — through every layer an operator can set them in:
// a v1 flat file, a v2 nested file, the ADR-0101 store and a CAMBRIAN_* variable.

func TestRetiredEpisodicKeys_StillLoad(t *testing.T) {
	cases := map[string]string{
		"v1 flat": `{"execution": {
			"consolidation_threshold_doc_count": 5000,
			"consolidation_threshold_bytes": 1073741824,
			"ewma_alpha": 0.42
		}}`,
		"v2 nested": `{"schema_version": 2, "execution": {
			"memory": {"consolidation_threshold_doc_count": 5000, "consolidation_threshold_bytes": 1073741824},
			"supervision": {"ewma_alpha": 0.42}
		}}`,
	}
	for name, tuning := range cases {
		t.Run(name, func(t *testing.T) {
			dir := baseBundle(t)
			writeCfg(t, dir, "tuning.json", tuning)
			t.Setenv("CAMBRIAN_EXECUTION__CONSOLIDATION_THRESHOLD_DOC_COUNT", "7")
			t.Setenv("CAMBRIAN_EXECUTION__MEMORY__CONSOLIDATION_THRESHOLD_BYTES", "9")
			store := mapStore{
				"execution.consolidation_threshold_doc_count":         float64(11),
				"execution.memory.consolidation_threshold_bytes":      float64(13),
				"execution.hippocampus.hippocampus_policies.episodic": map[string]any{"similarity": 0.65},
			}

			cfg, _ := loadFrom(t, dir, store)

			// The file was honoured around the retired keys: they do not poison
			// the layer they sit in.
			if got := cfg.Execution.Supervision.EWMAAlpha; got != 0.42 {
				t.Errorf("ewma_alpha from the same layer = %v, want 0.42", got)
			}
			if err := cfg.Execution.Validate(); err != nil {
				t.Errorf("Validate rejected a config carrying retired keys: %v", err)
			}
		})
	}
}

// An operator file that still declares the "episodic" policy — and even names it
// the default — keeps loading and validating: the policy map accepts any name.
func TestRetiredEpisodicPolicy_OperatorDeclaredStillValid(t *testing.T) {
	dir := baseBundle(t)
	writeCfg(t, dir, "tuning.json", `{"execution": {"hippocampus": {
		"hippocampus_policies": {"episodic": {"similarity": 0.65, "confidence": 0, "max_age_hours": 8760}},
		"hippocampus_default_policy": "episodic"
	}}}`)
	cfg, _ := loadFrom(t, dir, nil)
	if err := cfg.Execution.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if _, ok := cfg.Execution.Hippocampus.HippocampusPolicies["episodic"]; !ok {
		t.Error("operator-declared episodic policy was dropped")
	}
}

// The retired keys must be gone from the schema, not merely unused: a field that
// comes back would be a declared-but-unread knob again (AGENTS.md §3.4).
func TestRetiredEpisodicKeys_AbsentFromSchema(t *testing.T) {
	retired := map[string]bool{
		"consolidation_threshold_doc_count": true,
		"consolidation_threshold_bytes":     true,
	}
	var walk func(ty reflect.Type, path string)
	walk = func(ty reflect.Type, path string) {
		for i := range ty.NumField() {
			f := ty.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if retired[name] {
				t.Errorf("retired key %s.%s is back in ExecutionConfig", path, name)
			}
			if f.Type.Kind() == reflect.Struct {
				walk(f.Type, path+"."+name)
			}
		}
	}
	walk(reflect.TypeOf(ExecutionConfig{}), "execution")
	for k := range retired {
		if _, ok := legacyExecutionKey[k]; ok {
			t.Errorf("retired key %q still has a v1→v2 migration route", k)
		}
	}
	if _, ok := DefaultConfig().Execution.Hippocampus.HippocampusPolicies["episodic"]; ok {
		t.Error(`DefaultConfig still ships the retired "episodic" HippocampusPolicy`)
	}
}
