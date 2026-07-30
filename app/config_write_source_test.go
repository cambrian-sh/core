package app

import (
	"math"
	"path/filepath"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/storage"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

func writeSource(t *testing.T, dir string, applied map[string]float64) configWriteSource {
	t.Helper()
	store, err := storage.OpenConfigStore(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatalf("OpenConfigStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	_, prov, err := config.LoadConfigWithStore(filepath.Join(dir, "config.json"), store)
	if err != nil {
		t.Fatalf("LoadConfigWithStore: %v", err)
	}
	return configWriteSource{
		store: store,
		prov:  prov,
		hotApply: func(param string, v float64) bool {
			if applied == nil {
				return false
			}
			applied[param] = v
			return true
		},
	}
}

func outcomeFor(t *testing.T, outs []operator.ConfigWriteOutcome, key string) operator.ConfigWriteOutcome {
	t.Helper()
	for _, o := range outs {
		if o.Key == key {
			return o
		}
	}
	t.Fatalf("no outcome for %q in %+v", key, outs)
	return operator.ConfigWriteOutcome{}
}

// The property this whole write path exists for: an env var that will shadow the
// write must be reported AT WRITE TIME, naming the variable.
//
// Without it, the operator saves, sees success, observes nothing change, and has
// nothing anywhere in the product that explains why.
func TestSetConfig_ShadowedWriteIsReportedAtWriteTime(t *testing.T) {
	dir := bundle(t, map[string]string{})
	t.Setenv("CAMBRIAN_EXECUTION__EWMA_ALPHA", "0.42")

	src := writeSource(t, dir, nil)
	outs, err := src.SetConfig(map[string]float64{"execution.ewma_alpha": 0.8})
	if err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	o := outcomeFor(t, outs, "execution.ewma_alpha")
	if o.Effect != operator.EffectShadowed {
		t.Fatalf("effect = %q, want %q", o.Effect, operator.EffectShadowed)
	}
	if o.ShadowedBy != config.EnvSource("CAMBRIAN_EXECUTION__EWMA_ALPHA") {
		t.Fatalf("shadowed_by = %q — it must NAME the variable, not just say one exists", o.ShadowedBy)
	}
	// Still stored: it is the operator's stated intent and takes effect the
	// moment the variable is removed.
	if !o.Set {
		t.Fatal("a shadowed write must still be STORED — otherwise removing the env var silently reverts the operator's intent")
	}
}

// A hot-appliable key reports "live" and actually reaches the running kernel.
func TestSetConfig_LiveKeyIsAppliedAndStored(t *testing.T) {
	dir := bundle(t, map[string]string{})
	applied := map[string]float64{}
	src := writeSource(t, dir, applied)

	outs, err := src.SetConfig(map[string]float64{"execution.blend_weight_cosine": 0.6})
	if err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	o := outcomeFor(t, outs, "execution.blend_weight_cosine")
	if o.Effect != operator.EffectLive || !o.Set {
		t.Fatalf("effect = %q set = %v, want live/true", o.Effect, o.Set)
	}
	if applied["blend_weight_cosine"] != 0.6 {
		t.Fatalf("hot-apply received %v, want 0.6", applied["blend_weight_cosine"])
	}

	// And it survives: this is what SetRuntimeConfig could never do.
	overrides, _ := src.store.Overrides()
	if overrides["execution.blend_weight_cosine"] != 0.6 {
		t.Fatalf("stored value = %#v, want 0.6 — the write must outlive the process", overrides["execution.blend_weight_cosine"])
	}
}

// A key with no live path is stored and honestly reported as restart-required,
// rather than claimed as applied.
func TestSetConfig_KeyWithoutLivePathIsRestartRequired(t *testing.T) {
	dir := bundle(t, map[string]string{})
	src := writeSource(t, dir, map[string]float64{})

	outs, err := src.SetConfig(map[string]float64{"execution.ewma_alpha": 0.7})
	if err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	o := outcomeFor(t, outs, "execution.ewma_alpha")
	if o.Effect != operator.EffectRestartRequired || !o.Set {
		t.Fatalf("effect = %q set = %v, want restart_required/true", o.Effect, o.Set)
	}
}

// An out-of-range value is refused, not stored. A blend weight of 50 would wreck
// retrieval silently — nothing downstream validates it.
func TestSetConfig_OutOfRangeIsRejectedAndNotStored(t *testing.T) {
	dir := bundle(t, map[string]string{})
	src := writeSource(t, dir, map[string]float64{})

	outs, _ := src.SetConfig(map[string]float64{"execution.blend_weight_cosine": 50})
	o := outcomeFor(t, outs, "execution.blend_weight_cosine")

	if o.Effect != operator.EffectRejected || o.Set {
		t.Fatalf("effect = %q set = %v, want rejected/false", o.Effect, o.Set)
	}
	if o.Error == "" {
		t.Fatal("a rejection with no reason is not actionable")
	}
	overrides, _ := src.store.Overrides()
	if _, present := overrides["execution.blend_weight_cosine"]; present {
		t.Fatal("a rejected value reached the store")
	}
}

func TestSetConfig_NonFiniteIsRejected(t *testing.T) {
	dir := bundle(t, map[string]string{})
	src := writeSource(t, dir, map[string]float64{})

	for name, v := range map[string]float64{"NaN": math.NaN(), "Inf": math.Inf(1)} {
		outs, _ := src.SetConfig(map[string]float64{"execution.blend_weight_cosine": v})
		if o := outcomeFor(t, outs, "execution.blend_weight_cosine"); o.Effect != operator.EffectRejected {
			t.Fatalf("%s: effect = %q, want rejected", name, o.Effect)
		}
	}
}

// One bad key must not lose the good writes in the same request. A console built
// against a different kernel revision will send keys this one does not know.
func TestSetConfig_UnknownKeySkipsWithoutLosingValidWrites(t *testing.T) {
	dir := bundle(t, map[string]string{})
	src := writeSource(t, dir, map[string]float64{})

	outs, err := src.SetConfig(map[string]float64{
		"execution.blend_weight_cosine": 0.5,
		"execution.retired_axis":        0.9,
	})
	if err != nil {
		t.Fatalf("one unknown key failed the whole request: %v", err)
	}
	if o := outcomeFor(t, outs, "execution.retired_axis"); o.Effect != operator.EffectRejected || o.Set {
		t.Fatalf("unknown key: effect = %q set = %v, want rejected/false", o.Effect, o.Set)
	}
	if o := outcomeFor(t, outs, "execution.blend_weight_cosine"); !o.Set {
		t.Fatal("the valid write in the same request was lost")
	}
}

// Delete must actually unpin, so the layer beneath takes over again. Writing the
// old value back would leave the store pinning the key, and a later edit to the
// file underneath would silently do nothing.
func TestDeleteConfig_UnpinsTheKey(t *testing.T) {
	dir := bundle(t, map[string]string{})
	src := writeSource(t, dir, map[string]float64{})

	if _, err := src.SetConfig(map[string]float64{"execution.ewma_alpha": 0.7}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if _, err := src.DeleteConfig([]string{"execution.ewma_alpha"}); err != nil {
		t.Fatalf("DeleteConfig: %v", err)
	}

	overrides, _ := src.store.Overrides()
	if _, present := overrides["execution.ewma_alpha"]; present {
		t.Fatal("the key is still pinned by the store after delete")
	}
}

func TestDeleteConfig_AbsentKeyIsNotAnError(t *testing.T) {
	dir := bundle(t, map[string]string{})
	src := writeSource(t, dir, map[string]float64{})

	outs, err := src.DeleteConfig([]string{"execution.ewma_alpha"})
	if err != nil {
		t.Fatalf("deleting an absent key errored: %v", err)
	}
	if o := outcomeFor(t, outs, "execution.ewma_alpha"); !o.Set {
		t.Fatal("deleting an absent key should report success — the post-condition already holds")
	}
}

// ── credentials ──────────────────────────────────────────────────────────────

func TestSetGeneratorKey_StoresEncryptedAndReportsLive(t *testing.T) {
	dir := bundle(t, map[string]string{})
	src := writeSource(t, dir, nil)
	src.generators = func() map[string]string { return map[string]string{"gpt": "OPENAI_API_KEY"} }

	o, err := src.SetGeneratorKey("gpt", "sk-live-ABCD1234")
	if err != nil {
		t.Fatalf("SetGeneratorKey: %v", err)
	}
	if o.Effect != operator.EffectLive || !o.Set {
		t.Fatalf("effect = %q set = %v, want live/true", o.Effect, o.Set)
	}
	// Readable only as its last four — never as itself.
	if got := src.store.LastFour("generator:gpt:api_key"); got != "1234" {
		t.Fatalf("LastFour = %q, want 1234", got)
	}
}

// The credential case of the shadow rule, and it bites harder than the config
// case: an operator pasting a new key while the env var still holds the old one
// would otherwise see "saved" and keep getting auth failures from the old key.
func TestSetGeneratorKey_ShadowedByEnvIsReported(t *testing.T) {
	dir := bundle(t, map[string]string{})
	t.Setenv("OPENAI_API_KEY", "sk-from-env")

	src := writeSource(t, dir, nil)
	src.generators = func() map[string]string { return map[string]string{"gpt": "OPENAI_API_KEY"} }

	o, err := src.SetGeneratorKey("gpt", "sk-live-ABCD1234")
	if err != nil {
		t.Fatalf("SetGeneratorKey: %v", err)
	}
	if o.Effect != operator.EffectShadowed || o.ShadowedBy != "env:OPENAI_API_KEY" {
		t.Fatalf("effect = %q shadowed_by = %q, want shadowed/env:OPENAI_API_KEY", o.Effect, o.ShadowedBy)
	}
}

// A key filed against a generator that does not exist is invisible: it is never
// used, nothing errors, and the operator believes the provider is configured.
func TestSetGeneratorKey_UnknownGeneratorIsRefused(t *testing.T) {
	dir := bundle(t, map[string]string{})
	src := writeSource(t, dir, nil)
	src.generators = func() map[string]string { return map[string]string{"gpt": "OPENAI_API_KEY"} }

	o, err := src.SetGeneratorKey("nope", "sk-live-ABCD1234")
	if err != nil {
		t.Fatalf("SetGeneratorKey: %v", err)
	}
	if o.Effect != operator.EffectRejected || o.Set {
		t.Fatalf("effect = %q set = %v, want rejected/false", o.Effect, o.Set)
	}
	if src.store.Configured("generator:nope:api_key") {
		t.Fatal("a credential was stored against a generator that does not exist")
	}
}

func TestClearGeneratorKey_RemovesIt(t *testing.T) {
	dir := bundle(t, map[string]string{})
	src := writeSource(t, dir, nil)
	src.generators = func() map[string]string { return map[string]string{"gpt": "OPENAI_API_KEY"} }

	if _, err := src.SetGeneratorKey("gpt", "sk-live-ABCD1234"); err != nil {
		t.Fatalf("SetGeneratorKey: %v", err)
	}
	if err := src.ClearGeneratorKey("gpt"); err != nil {
		t.Fatalf("ClearGeneratorKey: %v", err)
	}
	if src.store.Configured("generator:gpt:api_key") {
		t.Fatal("still configured after clear")
	}
}

// End-to-end: a durable write must be visible to the NEXT boot's config load.
// This is the claim that separates SetConfig from SetRuntimeConfig, and nothing
// else in the suite proves it.
func TestSetConfig_SurvivesIntoTheNextConfigLoad(t *testing.T) {
	dir := bundle(t, map[string]string{})
	storePath := filepath.Join(t.TempDir(), "config.db")

	store, err := storage.OpenConfigStore(storePath)
	if err != nil {
		t.Fatalf("OpenConfigStore: %v", err)
	}
	_, prov, err := config.LoadConfigWithStore(filepath.Join(dir, "config.json"), store)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	src := configWriteSource{store: store, prov: prov, hotApply: func(string, float64) bool { return false }}
	if _, err := src.SetConfig(map[string]float64{"execution.ewma_alpha": 0.73}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	_ = store.Close()

	// A new process: reopen the store and reload config through it.
	store2, err := storage.OpenConfigStore(storePath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = store2.Close() }()

	cfg2, prov2, err := config.LoadConfigWithStore(filepath.Join(dir, "config.json"), store2)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if cfg2.Execution.EWMAAlpha != 0.73 {
		t.Fatalf("EWMAAlpha after restart = %v, want 0.73", cfg2.Execution.EWMAAlpha)
	}
	if got := prov2.Source("execution.ewma_alpha"); got != config.SourceStore {
		t.Fatalf("value_source = %q, want %q", got, config.SourceStore)
	}
}
