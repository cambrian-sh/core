package config

import (
	stdjson "encoding/json"
	"os"
	"testing"
)

// The fixed 120 s plan ceiling (execution.plan.plan_timeout_ms) was replaced on
// 2026-11-05 by plan_idle_timeout_ms + plan_max_duration_ms (owner decision,
// T-20261105-owner-decision-needed-120-s-plan-timeout-ms). An operator who set
// plan_timeout_ms chose a hard cap, so it loads as plan_max_duration_ms — through
// every layer an operator can set it in — and the idle watchdog keeps its default.

func TestDeprecatedPlanTimeout_LoadsAsMaxDuration(t *testing.T) {
	cases := map[string]struct {
		tuning string
		env    map[string]string
		store  mapStore
		want   int
	}{
		"v1 flat file":   {tuning: `{"execution": {"plan_timeout_ms": 600000}}`, want: 600000},
		"v2 nested file": {tuning: `{"schema_version": 2, "execution": {"plan": {"plan_timeout_ms": 450000}}}`, want: 450000},
		"store, v1 flat key": {
			store: mapStore{"execution.plan_timeout_ms": float64(300000)}, want: 300000,
		},
		"store, v2 key": {
			store: mapStore{"execution.plan.plan_timeout_ms": float64(310000)}, want: 310000,
		},
		"env, v1 flat name": {
			env: map[string]string{"CAMBRIAN_EXECUTION__PLAN_TIMEOUT_MS": "240000"}, want: 240000,
		},
		"env, v2 name": {
			env: map[string]string{"CAMBRIAN_EXECUTION__PLAN__PLAN_TIMEOUT_MS": "250000"}, want: 250000,
		},
		// The successor stated in the same layer is the newer intent and wins.
		"both in one file": {
			tuning: `{"execution": {"plan": {"plan_timeout_ms": 120000, "plan_max_duration_ms": 900000}}}`,
			want:   900000,
		},
		"both env vars": {
			env: map[string]string{
				"CAMBRIAN_EXECUTION__PLAN__PLAN_TIMEOUT_MS":      "120000",
				"CAMBRIAN_EXECUTION__PLAN__PLAN_MAX_DURATION_MS": "700000",
			},
			want: 700000,
		},
		// A higher layer's deprecated key still beats a lower layer's successor:
		// each layer is migrated on its own, exactly like the v1→v2 move.
		"deprecated in a higher layer": {
			tuning: `{"execution": {"plan": {"plan_max_duration_ms": 900000}}}`,
			store:  mapStore{"execution.plan.plan_timeout_ms": float64(200000)},
			want:   200000,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := baseBundle(t)
			if tc.tuning != "" {
				writeCfg(t, dir, "tuning.json", tc.tuning)
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			var store Store
			if tc.store != nil {
				store = tc.store
			}
			cfg, _ := loadFrom(t, dir, store)
			if got := cfg.Execution.Plan.PlanMaxDurationMs; got != tc.want {
				t.Errorf("plan_max_duration_ms = %d, want %d", got, tc.want)
			}
			if got := cfg.Execution.Plan.PlanIdleTimeoutMs; got != DefaultPlanIdleTimeoutMs {
				t.Errorf("plan_idle_timeout_ms = %d, want the default %d", got, DefaultPlanIdleTimeoutMs)
			}
			if err := cfg.Execution.Validate(); err != nil {
				t.Errorf("Validate: %v", err)
			}
		})
	}
}

// With no plan_timeout_ms anywhere the new behaviour is the default.
func TestPlanCeilingDefaults(t *testing.T) {
	cfg, _ := loadFrom(t, baseBundle(t), nil)
	if got := cfg.Execution.Plan.PlanIdleTimeoutMs; got != 180000 {
		t.Errorf("plan_idle_timeout_ms default = %d, want 180000", got)
	}
	if got := cfg.Execution.Plan.PlanMaxDurationMs; got != 1800000 {
		t.Errorf("plan_max_duration_ms default = %d, want 1800000", got)
	}
}

// The frozen v1 golden (execution_legacy_defaults.json) states plan_timeout_ms:
// 120000 explicitly, so a deployment running that file as its config keeps its
// 120 s HARD cap — now as plan_max_duration_ms — with the idle watchdog on top.
func TestDeprecatedPlanTimeout_FrozenV1GoldenStillLoads(t *testing.T) {
	raw, err := os.ReadFile(legacyGoldenPath)
	if err != nil {
		t.Fatalf("read %s: %v", legacyGoldenPath, err)
	}
	dir := baseBundle(t)
	writeCfg(t, dir, "tuning.json", `{"execution": `+string(raw)+`}`)
	cfg, _ := loadFrom(t, dir, nil)
	if got := cfg.Execution.Plan.PlanMaxDurationMs; got != 120000 {
		t.Errorf("plan_max_duration_ms = %d, want 120000 (the golden's explicit plan_timeout_ms)", got)
	}
}

// The rename is idempotent and leaves no deprecated key behind.
func TestDeprecatedPlanTimeout_MigrationIsIdempotent(t *testing.T) {
	in := []byte(`{"execution": {"plan_timeout_ms": 5000}}`)
	once := migrateJSON(in, "test")
	twice := migrateJSON(once, "test")
	if string(once) != string(twice) {
		t.Fatalf("not idempotent:\n once=%s\ntwice=%s", once, twice)
	}
	var doc map[string]any
	if err := stdjson.Unmarshal(once, &doc); err != nil {
		t.Fatal(err)
	}
	plan := doc["execution"].(map[string]any)["plan"].(map[string]any)
	if _, left := plan["plan_timeout_ms"]; left {
		t.Error("plan_timeout_ms left in the migrated document")
	}
	if plan["plan_max_duration_ms"] != 5000.0 {
		t.Errorf("plan_max_duration_ms = %v, want 5000", plan["plan_max_duration_ms"])
	}
}
