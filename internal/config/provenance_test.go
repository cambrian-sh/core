package config

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func formatFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// writeCfg writes a config bundle file into dir and returns nothing; a helper so
// the layering tests read as "these files exist" rather than as file plumbing.
func writeCfg(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// baseBundle writes the minimum that makes validateSecrets pass, so a test can
// assert on layering rather than on required-field errors.
func baseBundle(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeCfg(t, dir, "config.json", `{
	  "database": {"host": "localhost", "user": "u", "password": "p", "dbname": "d"}
	}`)
	return dir
}

func loadFrom(t *testing.T, dir string, store Store) (*Config, Provenance) {
	t.Helper()
	cfg, prov, err := LoadConfigWithStore(filepath.Join(dir, "config.json"), store)
	if err != nil {
		t.Fatalf("LoadConfigWithStore: %v", err)
	}
	return cfg, prov
}

func TestProvenance_DefaultsAttributedToDefault(t *testing.T) {
	dir := baseBundle(t)
	_, prov := loadFrom(t, dir, nil)

	if got := prov.Source("execution.ewma_alpha"); got != SourceDefault {
		t.Fatalf("ewma_alpha source = %q, want %q", got, SourceDefault)
	}
	// The key the test bundle DID set must not be attributed to defaults.
	if got := prov.Source("database.host"); got != SourceConfig {
		t.Fatalf("database.host source = %q, want %q", got, SourceConfig)
	}
}

// The highest-priority layer that sets a key must win, not the last layer that
// merely mentions it. This is the distinction that makes value_source useful:
// tuning.json and tuning.local.json both name ewma_alpha, and only one of them
// is the value the kernel runs on.
func TestProvenance_HighestLayerWins(t *testing.T) {
	dir := baseBundle(t)
	writeCfg(t, dir, "tuning.json", `{"execution": {"ewma_alpha": 0.5}}`)
	writeCfg(t, dir, "tuning.local.json", `{"execution": {"ewma_alpha": 0.9}}`)

	cfg, prov := loadFrom(t, dir, nil)

	if cfg.Execution.EWMAAlpha != 0.9 {
		t.Fatalf("EWMAAlpha = %v, want 0.9", cfg.Execution.EWMAAlpha)
	}
	if got := prov.Source("execution.ewma_alpha"); got != SourceTuningLocal {
		t.Fatalf("source = %q, want %q", got, SourceTuningLocal)
	}
}

// Attribution is by PRESENCE, not by value change. When two layers state the
// same value, the higher one owns the key — because that is the file an operator
// has to edit to change it, and the file whose removal would change the answer.
//
// Getting this wrong is not cosmetic: a diff-based tracker reports the LOWER
// layer here, so an operator reads "tuning.json" and edits a file that is being
// overridden by the one they were not told about.
func TestProvenance_HighestStatingLayerOwnsEvenWhenValuesMatch(t *testing.T) {
	dir := baseBundle(t)
	writeCfg(t, dir, "tuning.json", `{"execution": {"ewma_alpha": 0.5}}`)
	writeCfg(t, dir, "tuning.local.json", `{"execution": {"ewma_alpha": 0.5}}`)

	_, prov := loadFrom(t, dir, nil)

	if got := prov.Source("execution.ewma_alpha"); got != SourceTuningLocal {
		t.Fatalf("source = %q, want %q (the highest layer stating the key)", got, SourceTuningLocal)
	}
}

// The same rule against the Go defaults, which is where a diff-based tracker
// fails most visibly: a file that pins a key to exactly its default value must
// still be reported as the owner, or the operator is told "default" for a key
// their own config file controls.
func TestProvenance_FileRestatingADefaultStillOwnsIt(t *testing.T) {
	dir := baseBundle(t)
	def := DefaultConfig()
	writeCfg(t, dir, "tuning.json", `{"execution": {"ewma_alpha": `+
		formatFloat(def.Execution.EWMAAlpha)+`}}`)

	_, prov := loadFrom(t, dir, nil)

	if got := prov.Source("execution.ewma_alpha"); got != SourceTuning {
		t.Fatalf("source = %q, want %q — a file pinning a default still owns the key", got, SourceTuning)
	}
}

// A layer that does not mention a key must not claim it.
func TestProvenance_SilentLayerClaimsNothing(t *testing.T) {
	dir := baseBundle(t)
	writeCfg(t, dir, "tuning.json", `{"execution": {"ewma_alpha": 0.5}}`)
	writeCfg(t, dir, "tuning.local.json", `{"execution": {"latency_window_size": 7}}`)

	_, prov := loadFrom(t, dir, nil)

	if got := prov.Source("execution.ewma_alpha"); got != SourceTuning {
		t.Fatalf("source = %q, want %q — tuning.local.json never mentions this key", got, SourceTuning)
	}
	if got := prov.Source("execution.latency_window_size"); got != SourceTuningLocal {
		t.Fatalf("source = %q, want %q", got, SourceTuningLocal)
	}
}

func TestProvenance_EnvNamesItsVariable(t *testing.T) {
	dir := baseBundle(t)
	writeCfg(t, dir, "tuning.json", `{"execution": {"ewma_alpha": 0.5}}`)
	t.Setenv("CAMBRIAN_EXECUTION__EWMA_ALPHA", "0.77")

	cfg, prov := loadFrom(t, dir, nil)

	if cfg.Execution.EWMAAlpha != 0.77 {
		t.Fatalf("EWMAAlpha = %v, want 0.77", cfg.Execution.EWMAAlpha)
	}
	want := EnvSource("CAMBRIAN_EXECUTION__EWMA_ALPHA")
	if got := prov.Source("execution.ewma_alpha"); got != want {
		t.Fatalf("source = %q, want %q", got, want)
	}
	// PinnedAbove is what the write path calls to warn at save time (D3).
	if got := prov.PinnedAbove("execution.ewma_alpha"); got != want {
		t.Fatalf("PinnedAbove = %q, want %q", got, want)
	}
}

// A key nothing above the store supplies must report as NOT pinned, or every
// write would carry a spurious warning and operators would learn to ignore it.
func TestProvenance_PinnedAboveIsEmptyForUnpinnedKey(t *testing.T) {
	dir := baseBundle(t)
	_, prov := loadFrom(t, dir, nil)

	if got := prov.PinnedAbove("execution.ewma_alpha"); got != "" {
		t.Fatalf("PinnedAbove = %q, want \"\"", got)
	}
	if got := prov.PinnedAbove("no.such.key"); got != "" {
		t.Fatalf("PinnedAbove(unknown) = %q, want \"\"", got)
	}
}

// ── the store layer ──────────────────────────────────────────────────────────

// mapStore is an in-memory Store: enough to pin the LAYERING without dragging
// bbolt into the config package's tests.
type mapStore map[string]any

func (m mapStore) Overrides() (map[string]any, error) { return m, nil }
func (m mapStore) SetOverride(k string, v any) error  { m[k] = v; return nil }
func (m mapStore) DeleteOverride(k string) error      { delete(m, k); return nil }

func TestStoreLayer_BeatsFiles(t *testing.T) {
	dir := baseBundle(t)
	writeCfg(t, dir, "tuning.json", `{"execution": {"ewma_alpha": 0.5}}`)
	writeCfg(t, dir, "tuning.local.json", `{"execution": {"ewma_alpha": 0.6}}`)

	cfg, prov := loadFrom(t, dir, mapStore{"execution.ewma_alpha": 0.8})

	if cfg.Execution.EWMAAlpha != 0.8 {
		t.Fatalf("EWMAAlpha = %v, want 0.8 (store outranks every file)", cfg.Execution.EWMAAlpha)
	}
	if got := prov.Source("execution.ewma_alpha"); got != SourceStore {
		t.Fatalf("source = %q, want %q", got, SourceStore)
	}
}

// ADR-0101 D1's load-bearing half: a deployment configured by environment must
// behave identically after the store lands. If this ever fails, containers, CI
// and the benchmark rig have all silently changed behaviour.
func TestStoreLayer_EnvStillWins(t *testing.T) {
	dir := baseBundle(t)
	t.Setenv("CAMBRIAN_EXECUTION__EWMA_ALPHA", "0.99")

	cfg, prov := loadFrom(t, dir, mapStore{"execution.ewma_alpha": 0.8})

	if cfg.Execution.EWMAAlpha != 0.99 {
		t.Fatalf("EWMAAlpha = %v, want 0.99 (env outranks the store)", cfg.Execution.EWMAAlpha)
	}
	want := EnvSource("CAMBRIAN_EXECUTION__EWMA_ALPHA")
	if got := prov.PinnedAbove("execution.ewma_alpha"); got != want {
		t.Fatalf("PinnedAbove = %q, want %q — the operator must be told at write time", got, want)
	}
}

// A nil store must reproduce the pre-ADR-0101 pipeline exactly.
func TestStoreLayer_NilStoreIsThePreviousPipeline(t *testing.T) {
	dir := baseBundle(t)
	writeCfg(t, dir, "tuning.json", `{"execution": {"ewma_alpha": 0.5}}`)

	cfg, prov := loadFrom(t, dir, nil)

	if cfg.Execution.EWMAAlpha != 0.5 {
		t.Fatalf("EWMAAlpha = %v, want 0.5", cfg.Execution.EWMAAlpha)
	}
	if got := prov.Source("execution.ewma_alpha"); got != SourceTuning {
		t.Fatalf("source = %q, want %q — no store layer should appear", got, SourceTuning)
	}
}

// An empty store must not claim keys it never set: otherwise every default in
// the kernel would report "store" and the pin warning would fire on everything.
func TestStoreLayer_EmptyStoreClaimsNothing(t *testing.T) {
	dir := baseBundle(t)
	_, prov := loadFrom(t, dir, mapStore{})

	if got := prov.Source("execution.ewma_alpha"); got != SourceDefault {
		t.Fatalf("source = %q, want %q", got, SourceDefault)
	}
}

// ── expand ───────────────────────────────────────────────────────────────────

func TestExpand_NestsDottedKeys(t *testing.T) {
	got := expand(map[string]any{"a.b.c": 1, "a.b.d": 2, "e": 3})

	a, ok := got["a"].(map[string]any)
	if !ok {
		t.Fatalf("a is %T, want map", got["a"])
	}
	b, ok := a["b"].(map[string]any)
	if !ok {
		t.Fatalf("a.b is %T, want map", a["b"])
	}
	if b["c"] != 1 || b["d"] != 2 || got["e"] != 3 {
		t.Fatalf("expand produced %#v", got)
	}
}

// A scalar and a subtree cannot both occupy one path. Guessing a merge would
// corrupt the neighbouring value, so the colliding key is skipped instead.
func TestExpand_ScalarSubtreeCollisionSkips(t *testing.T) {
	got := expand(map[string]any{"a": 1, "a.b": 2})

	if v, ok := got["a"].(map[string]any); ok {
		t.Fatalf("scalar was overwritten by a subtree: %#v", v)
	}
	if got["a"] != 1 {
		t.Fatalf("a = %v, want the scalar 1 preserved", got["a"])
	}
}
