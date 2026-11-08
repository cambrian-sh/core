package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Boot validation (T-20261106-executionconfig-validate-has-no-production):
// ExecutionConfig.Validate and ChunkerConfig.Validate had no production caller,
// so every execution.* and chunker rule was dead at load. These tests pin that
// LoadConfigWithStore now runs them, after the merge and the v1→v2 migration.

func loadErr(t *testing.T, dir string, store Store) string {
	t.Helper()
	_, _, err := LoadConfigWithStore(filepath.Join(dir, "config.json"), store)
	if err == nil {
		t.Fatal("LoadConfigWithStore accepted an invalid config")
	}
	return err.Error()
}

func TestLoad_InvalidExecutionValueFailsBootNamingTheKey(t *testing.T) {
	dir := baseBundle(t)
	// v1 flat spelling in a file layer: migrated first, then judged.
	writeCfg(t, dir, "tuning.json", `{"execution": {"ewma_alpha": 2}}`)
	msg := loadErr(t, dir, nil)
	if !strings.Contains(msg, "execution.supervision.ewma_alpha must be in [0, 1]") {
		t.Fatalf("error does not name the full key: %s", msg)
	}
}

func TestLoad_ValidationErrorsAreAggregated(t *testing.T) {
	dir := baseBundle(t)
	writeCfg(t, dir, "tuning.json", `{"execution": {
	  "retrieval": {"hnsw_iterative_scan": "sideways"},
	  "hooks": {"entries": [{"name": "h", "on_failure": "maybe"}]}
	}}`)
	writeCfg(t, dir, "config.local.json", `{"chunker": {"default": "option_c", "routes": {"pdf": "nope"}}}`)
	msg := loadErr(t, dir, nil)
	for _, want := range []string{
		"execution.retrieval.hnsw_iterative_scan",
		"execution.hooks.entries[0].on_failure",
		`chunker.routes["pdf"]`,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("aggregated error is missing %q: %s", want, msg)
		}
	}
}

func TestLoad_InvalidEnvValueFailsBoot(t *testing.T) {
	dir := baseBundle(t)
	t.Setenv("CAMBRIAN_EXECUTION__VERIFICATION__CROSS_VERIFY_RATE", "1.5")
	if msg := loadErr(t, dir, nil); !strings.Contains(msg, "execution.verification.cross_verify_rate") {
		t.Fatalf("error does not name the key: %s", msg)
	}
}

func TestLoad_InvalidStoreOverrideFailsBoot(t *testing.T) {
	dir := baseBundle(t)
	msg := loadErr(t, dir, mapStore{"execution.max_replan_attempts": -1})
	if !strings.Contains(msg, "execution.plan.max_replan_attempts") {
		t.Fatalf("error does not name the key: %s", msg)
	}
}

// The effect-vocabulary check moved out of the secrets list into
// ExecutionConfig.Validate (one place); it must still fail boot.
func TestLoad_InvalidEffectVocabularyFailsBoot(t *testing.T) {
	dir := baseBundle(t)
	writeCfg(t, dir, "tuning.json", `{"execution": {"capability": {"effect_vocabulary": [{"name": ""}]}}}`)
	if msg := loadErr(t, dir, nil); !strings.Contains(msg, "execution.capability.effect_vocabulary") {
		t.Fatalf("error does not name the key: %s", msg)
	}
}

func TestValidate_DefaultsPass(t *testing.T) {
	d := DefaultConfig()
	if err := d.Execution.Validate(); err != nil {
		t.Fatalf("default execution config fails validation: %v", err)
	}
	if err := d.Chunker.Validate(); err != nil {
		t.Fatalf("default chunker config fails validation: %v", err)
	}
	// The minimal bundle (defaults + required database fields) boots.
	loadFrom(t, baseBundle(t), nil)
}

// The committed tuning.json must validate — it ships in every checkout.
func TestValidate_CommittedTuningPasses(t *testing.T) {
	dir := baseBundle(t)
	b, err := os.ReadFile(filepath.Join("..", "..", "configs", "tuning.json"))
	if err != nil {
		t.Skipf("configs/tuning.json not readable: %v", err)
	}
	writeCfg(t, dir, "tuning.json", string(b))
	loadFrom(t, dir, nil)
}

// The FROZEN v1 snapshot, loaded as an operator file through the real pipeline,
// still migrates to v2 and passes every load-time rule.
func TestValidate_FrozenLegacyFileMigratesAndValidates(t *testing.T) {
	raw, err := os.ReadFile(legacyGoldenPath)
	if err != nil {
		t.Fatalf("read %s: %v", legacyGoldenPath, err)
	}
	dir := baseBundle(t)
	writeCfg(t, dir, "tuning.json", `{"execution": `+string(raw)+`}`)
	cfg, _ := loadFrom(t, dir, nil)
	if err := cfg.Execution.Validate(); err != nil {
		t.Fatalf("migrated legacy config fails validation: %v", err)
	}
}

func TestValidateOverride_RejectsOnlyIntroducedErrors(t *testing.T) {
	dir := baseBundle(t)
	path := filepath.Join(dir, "config.json")
	if errs := ValidateOverride(path, mapStore{}, "execution.ewma_alpha", 0.5); len(errs) != 0 {
		t.Fatalf("valid write refused: %v", errs)
	}
	// Cross-field: each weight is in range alone, the sum is not.
	store := mapStore{"execution.verification.trust_score_cal_weight": 0.6}
	errs := ValidateOverride(path, store, "execution.trust_score_abs_weight", 0.6)
	if len(errs) != 1 || !strings.Contains(errs[0], "trust_score_cal_weight + trust_score_abs_weight") {
		t.Fatalf("cross-field violation not reported: %v", errs)
	}
	// A pending value replaces a stale override stored under the other spelling.
	stale := mapStore{"execution.ewma_alpha": 0.5}
	if errs := ValidateOverride(path, stale, "execution.supervision.ewma_alpha", 3); len(errs) == 0 {
		t.Fatal("pending override lost to a stale v1-spelled store entry")
	}
	// A problem the config already has is not blamed on an unrelated write.
	writeCfg(t, dir, "tuning.json", `{"execution": {"plan": {"max_replan_attempts": -1}}}`)
	if errs := ValidateOverride(path, mapStore{}, "execution.ewma_alpha", 0.5); len(errs) != 0 {
		t.Fatalf("pre-existing error blamed on an unrelated write: %v", errs)
	}
}
