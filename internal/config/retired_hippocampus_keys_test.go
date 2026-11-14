package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// ENG-1301 (owner decision 2026-11-14, third round): the ADR-0027 procedural plan
// memory is deleted, and its two keys retire through retiredConfigKeys. Every
// layer that still sets them — a v1 flat file, a v2 nested file, the ADR-0101 store
// and a CAMBRIAN_* variable naming a path INSIDE the policy map — keeps loading;
// the keys are dropped (no provenance, no unknown-key report, and no empty
// execution.hippocampus block left behind), and the layer's other keys still apply.
func TestRetiredHippocampusKeys_EveryLayerStillLoadsAndDropsThem(t *testing.T) {
	cases := map[string]string{
		"v1 flat": `{"execution": {
			"hippocampus_policies": {"codegen": {"similarity": 0.9}},
			"hippocampus_default_policy": "codegen",
			"ewma_alpha": 0.42
		}}`,
		"v2 nested": `{"schema_version": 2, "execution": {
			"hippocampus": {"hippocampus_policies": {"codegen": {"similarity": 0.9}}, "hippocampus_default_policy": "codegen"},
			"supervision": {"ewma_alpha": 0.42}
		}}`,
	}
	for name, tuning := range cases {
		t.Run(name, func(t *testing.T) {
			dir := baseBundle(t)
			writeCfg(t, dir, "tuning.json", tuning)
			t.Setenv("CAMBRIAN_EXECUTION__HIPPOCAMPUS__HIPPOCAMPUS_POLICIES__TOOL__SIMILARITY", "0.5")
			store := mapStore{"execution.hippocampus.hippocampus_default_policy": "tool"}

			cfg, prov, unused, err := loadMergedReport(filepath.Join(dir, "config.json"), store)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("a config carrying the retired hippocampus keys must still validate: %v", err)
			}
			if got := cfg.Execution.Supervision.EWMAAlpha; got != 0.42 {
				t.Errorf("ewma_alpha from the same layer = %v, want 0.42", got)
			}
			for key, src := range prov {
				if strings.Contains(key, "hippocampus") {
					t.Errorf("retired hippocampus key %s still claimed by %s", key, src)
				}
			}
			for _, k := range unused {
				if strings.Contains(k, "hippocampus") {
					t.Errorf("retired hippocampus key reported as unknown instead of dropped: %s", k)
				}
			}
		})
	}
}

func TestRetiredEnvKey_MatchesPathsInsideARetiredMap(t *testing.T) {
	for path, want := range map[string]bool{
		"execution.hippocampus.hippocampus_policies":                    true,
		"execution.hippocampus.hippocampus_policies.codegen.similarity": true,
		"execution.hippocampus_policies.codegen":                        true, // v1 flat spelling
		"execution.hippocampus.hippocampus_default_policy":              true,
		"execution.hippocampus.hippocampus_policies_extra":              false, // a prefix of the name, not a sub-path
		"execution.retrieval.recall_top_k":                              false,
	} {
		if got := retiredEnvKey(path); got != want {
			t.Errorf("retiredEnvKey(%q) = %v, want %v", path, got, want)
		}
	}
}
