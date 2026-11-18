package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Schema v3 (owner decision 2026-11-14, ENG-1502 / ENG-203) retired the keys in
// retiredConfigKeys. An existing deployment that still sets one must keep starting:
// every layer (a v1 flat file, a v2 nested file, the ADR-0101 store and a CAMBRIAN_*
// variable, by either spelling) is migrated v2→v3 on load, the key is dropped, and
// the same layer's other keys are honoured.
func TestRetiredV3Keys_EveryLayerStillLoadsAndDropsThem(t *testing.T) {
	cases := map[string]string{
		"v1 flat": `{"execution": {
			"max_recursion_depth": 5, "proposal_timeout_ms": 900, "ewma_alpha": 0.42
		}, "telemetry": {"otlp_endpoint": "collector:4317", "prometheus_port": 9464}}`,
		"v2 nested": `{"schema_version": 2, "execution": {
			"plan": {"max_recursion_depth": 5, "budget_exhaustion_alarm_rate": 0.2},
			"verification": {"verifier_pool_min_size": 3},
			"supervision": {"ewma_alpha": 0.42}
		}, "telemetry": {"otlp_endpoint": "collector:4317", "prometheus_port": 9464}}`,
	}
	for name, tuning := range cases {
		t.Run(name, func(t *testing.T) {
			dir := baseBundle(t)
			writeCfg(t, dir, "tuning.json", tuning)
			t.Setenv("CAMBRIAN_EXECUTION__MEMORY__REMEMBER_DEFAULT_ACTIVATION", "0.9")
			t.Setenv("CAMBRIAN_EXECUTION__LLM_GATEWAY_RETRY_BACKOFF_MS", "50") // v1 flat spelling
			t.Setenv("CAMBRIAN_TELEMETRY__PROMETHEUS_PORT", "9000")
			store := mapStore{
				"execution.memory.min_gc_age_days":                    float64(7),
				"execution.max_partial_context_bytes":                 float64(-1), // v1 flat, and invalid before v3
				"execution.verification.verifier_pool_threshold_step": 0.1,
			}

			cfg, prov, unused, err := loadMergedReport(filepath.Join(dir, "config.json"), store)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("a config carrying retired keys must still validate: %v", err)
			}
			if got := cfg.Execution.Supervision.EWMAAlpha; got != 0.42 {
				t.Errorf("ewma_alpha from the same layer = %v, want 0.42", got)
			}
			// Dropped, not merely ignored: no provenance and no unknown-key report,
			// so the retirement WARN is the only thing the operator sees about them.
			for key := range retiredConfigKeys {
				if src, ok := prov[key]; ok && src != SourceDefault {
					t.Errorf("retired key %s still claimed by %s", key, src)
				}
			}
			for _, k := range unused {
				for retired := range retiredConfigKeys {
					leaf := retired[strings.LastIndexByte(retired, '.')+1:]
					if strings.HasSuffix(k, "."+leaf) || k == leaf {
						t.Errorf("retired key %s reached the decoder (unused %q)", retired, k)
					}
				}
			}
		})
	}
}

// The retired keys are gone from the schema itself (a Config leaf that comes back
// would be a declared-but-unread knob again), and from the defaults.
func TestRetiredV3Keys_AbsentFromSchema(t *testing.T) {
	leaves := map[string]bool{}
	for _, l := range configLeaves() {
		leaves[l.key] = true
	}
	for key := range retiredConfigKeys {
		if leaves[key] {
			t.Errorf("retired key %s is a Config leaf again", key)
		}
		if flat := retiredFlatSpelling(key); flat != "" {
			if _, routed := legacyExecutionKey[strings.TrimPrefix(flat, "execution.")]; routed {
				t.Errorf("retired key %s is still routed by legacyExecutionKey", flat)
			}
		}
	}
	if CurrentSchemaVersion < SchemaV3Retired {
		t.Errorf("CurrentSchemaVersion = %d, want at least %d", CurrentSchemaVersion, SchemaV3Retired)
	}
}

// The explicit `config-migrate` removes the retired keys from the file and stamps
// v3; a second run changes nothing.
func TestMigrateFileInPlace_RemovesRetiredV3Keys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tuning.json")
	writeCfg(t, filepath.Dir(path), "tuning.json", `{"schema_version": 2, "execution": {
		"plan": {"max_recursion_depth": 5, "max_replan_attempts": 1}
	}, "telemetry": {"otlp_endpoint": "x", "enable_stdout_exporter": true}}`)

	got, err := MigrateFileInPlace(path)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	want := []string{"execution.plan.max_recursion_depth", "telemetry.otlp_endpoint"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("removed %v, want %v", got, want)
	}
	b, _ := os.ReadFile(path)
	for _, gone := range []string{"max_recursion_depth", "otlp_endpoint"} {
		if strings.Contains(string(b), gone) {
			t.Errorf("%s still in the migrated file:\n%s", gone, b)
		}
	}
	for _, kept := range []string{`"max_replan_attempts": 1`, `"enable_stdout_exporter": true`, `"schema_version": 4`} {
		if !strings.Contains(string(b), kept) {
			t.Errorf("%s missing from the migrated file:\n%s", kept, b)
		}
	}
	if again, err := MigrateFileInPlace(path); err != nil || len(again) != 0 {
		t.Errorf("second migrate = %v, %v; want nothing", again, err)
	}
}
