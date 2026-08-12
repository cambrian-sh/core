package app

import (
	"testing"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// The FIRST saved generator becomes the default automatically. Without this, a
// stored generator list with no llm_provider.default refuses the next boot —
// and the console that could undo the write needs a running kernel (ADR-0123
// constraint 2; bricked a real deployment on 2026-08-11).
func TestSaveGenerator_FirstGeneratorAutoDefaultsAndAppliesLive(t *testing.T) {
	dir := bundle(t, map[string]string{})
	src := writeSource(t, dir, nil)
	src.generatorList = func() []config.GeneratorConfig { return nil }
	src.defaultGeneratorID = func() string { return "" }

	var appliedGens []config.GeneratorConfig
	var appliedDefault string
	src.applyGenerators = func(gens []config.GeneratorConfig, def string) bool {
		appliedGens, appliedDefault = gens, def
		return true
	}

	o, err := src.SaveGenerator(operator.GeneratorSpec{ID: "g1", Provider: "openai", Model: "m"})
	if err != nil {
		t.Fatalf("SaveGenerator: %v", err)
	}
	if o.Effect != operator.EffectLive || !o.Set {
		t.Fatalf("effect = %q set = %v, want live/true", o.Effect, o.Set)
	}
	if appliedDefault != "g1" || len(appliedGens) != 1 {
		t.Fatalf("applied default = %q gens = %d, want g1/1", appliedDefault, len(appliedGens))
	}
	overrides, _ := src.store.Overrides()
	if got, _ := overrides[defaultGeneratorKey].(string); got != "g1" {
		t.Fatalf("stored default = %q, want g1", got)
	}

	// A SECOND generator must NOT steal the default.
	if _, err := src.SaveGenerator(operator.GeneratorSpec{ID: "g2", Provider: "openai", Model: "m2"}); err != nil {
		t.Fatalf("second SaveGenerator: %v", err)
	}
	if appliedDefault != "g1" {
		t.Fatalf("second save moved the default to %q; it must stay g1", appliedDefault)
	}
	if len(appliedGens) != 2 {
		t.Fatalf("second save applied %d generators, want 2", len(appliedGens))
	}
}

// With no live provider (applyGenerators nil), the write persists but reports
// restart-required — never a false "live".
func TestSaveGenerator_NoLiveProviderIsRestartRequired(t *testing.T) {
	dir := bundle(t, map[string]string{})
	src := writeSource(t, dir, nil)
	src.generatorList = func() []config.GeneratorConfig { return nil }

	o, err := src.SaveGenerator(operator.GeneratorSpec{ID: "g1", Provider: "openai", Model: "m"})
	if err != nil {
		t.Fatalf("SaveGenerator: %v", err)
	}
	if o.Effect != operator.EffectRestartRequired || !o.Set {
		t.Fatalf("effect = %q set = %v, want restart_required/true", o.Effect, o.Set)
	}
}

// The removal guard judges the EFFECTIVE default — including one minted by the
// auto-default — not the boot snapshot.
func TestRemoveGenerator_RefusesEffectiveDefaultAndRemovesOthersLive(t *testing.T) {
	dir := bundle(t, map[string]string{})
	src := writeSource(t, dir, nil)
	src.generatorList = func() []config.GeneratorConfig { return nil }
	src.defaultGeneratorID = func() string { return "" }
	live := true
	src.applyGenerators = func([]config.GeneratorConfig, string) bool { return live }

	if _, err := src.SaveGenerator(operator.GeneratorSpec{ID: "g1", Provider: "openai", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if _, err := src.SaveGenerator(operator.GeneratorSpec{ID: "g2", Provider: "openai", Model: "m2"}); err != nil {
		t.Fatal(err)
	}

	o, err := src.RemoveGenerator("g1")
	if err != nil {
		t.Fatalf("RemoveGenerator: %v", err)
	}
	if o.Effect != operator.EffectRejected {
		t.Fatalf("removing the auto-defaulted generator must be rejected, got %q", o.Effect)
	}

	o, err = src.RemoveGenerator("g2")
	if err != nil {
		t.Fatalf("RemoveGenerator g2: %v", err)
	}
	if o.Effect != operator.EffectLive || !o.Set {
		t.Fatalf("effect = %q set = %v, want live/true", o.Effect, o.Set)
	}
}
