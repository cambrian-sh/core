package config

import (
	"sort"

	koanf "github.com/knadh/koanf/v2"
)

// Source labels for the layers LoadConfig merges, lowest priority first. These
// strings cross the operator plane verbatim as ConfigSchemaOp.value_source, so
// they are a contract with the console: it renders "pinned by <source>" from
// them. Change one and the UI's copy changes with it (ADR-0101 D4).
const (
	SourceDefault        = "default"
	SourceTuning         = "tuning.json"
	SourceTuningLocal    = "tuning.local.json"
	SourceConfig         = "config.json"
	SourceConfigLocal    = "config.local.json"
	SourceEmbedder       = "embedder.json"
	SourceEmbedderLocal  = "embedder.local.json"
	SourceProviders      = "providers.json"
	SourceProvidersLocal = "providers.local.json"
	SourceMCP            = "mcp.json"
	SourceStore          = "store"
	SourceEnvPrefix      = "env:"
)

// EnvSource renders the value_source label for a key supplied by the
// environment. The variable name is part of the label because "an env var pins
// this" is not actionable — the operator needs to know WHICH one to unset.
func EnvSource(envVar string) string { return SourceEnvPrefix + envVar }

// Provenance maps a flat Koanf key ("execution.ewma_alpha") to the label of the
// layer that last set it. It answers the one question the merged *Config cannot:
// "I changed this and nothing happened — what is pinning it?"
//
// Absent key ⇒ the key is not part of the merged config at all. A key present in
// every layer resolves to the HIGHEST-priority layer that set it, which is by
// construction the layer whose value the kernel is actually using.
type Provenance map[string]string

// Source returns the layer that supplied key, or "" when the key is unknown.
func (p Provenance) Source(key string) string { return p[key] }

// Keys returns every tracked key in sorted order. Sorted rather than map order
// so a diff of two provenance dumps is readable.
func (p Provenance) Keys() []string {
	out := make([]string, 0, len(p))
	for k := range p {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PinnedAbove reports whether key is supplied by a layer that outranks the
// embedded store — i.e. whether a write to the store would be shadowed. It
// returns the pinning source, or "" when a store write would take effect.
//
// This is the read side of ADR-0101 D3: the store's write path calls it so the
// ack can say "stored, but CAMBRIAN_EXECUTION__EWMA_ALPHA is pinning this key"
// at write time, rather than leaving the operator to discover it on a later read.
func (p Provenance) PinnedAbove(key string) string {
	src, ok := p[key]
	if !ok {
		return ""
	}
	if len(src) >= len(SourceEnvPrefix) && src[:len(SourceEnvPrefix)] == SourceEnvPrefix {
		return src
	}
	return ""
}

// tracker records which layer supplies each key, by observing what each layer
// individually CONTAINS rather than by diffing the merged result.
//
// The distinction matters, and the obvious implementation gets it wrong.
// Diffing k.All() before and after a layer attributes only keys whose VALUE
// changed — so a tuning.json that explicitly sets ewma_alpha to the same number
// the Go default already held is reported as "default". The operator then reads
// "default" for a key their own file pins, which is exactly the confusion
// value_source exists to remove.
//
// Attributing by PRESENCE gives the right answer, because Koanf merges
// last-writer-wins per key: the highest layer that states a key is by
// construction the layer whose value survives into the merged config. So this
// still cannot disagree with what the kernel runs on (ADR-0101 D4) — it reads the
// same layer inputs, in the same order, under the same precedence.
type tracker struct {
	prov Provenance
	// envKeys records, per config key, the environment VARIABLE that supplied it,
	// so the label can name it. Populated only for the env layer.
	envKeys map[string]string
}

func newTracker() *tracker { return &tracker{prov: make(Provenance)} }

// claim attributes every key present in `layer` to `source`, overwriting any
// attribution from a lower layer. `layer` holds ONLY that layer's contents, and
// a nil layer (an absent file) claims nothing.
func (t *tracker) claim(layer *koanf.Koanf, source string) {
	if layer == nil {
		return
	}
	for key := range layer.All() {
		t.prov[key] = source
	}
}

// claimEnv attributes the environment layer, labelling each key with the
// variable that supplied it. The variable name is the actionable half: "an env
// var pins this" leaves the operator hunting, "CAMBRIAN_EXECUTION__EWMA_ALPHA
// pins this" does not.
func (t *tracker) claimEnv(layer *koanf.Koanf) {
	if layer == nil {
		return
	}
	for key := range layer.All() {
		if envVar, ok := t.envKeys[key]; ok {
			t.prov[key] = EnvSource(envVar)
			continue
		}
		t.prov[key] = SourceEnvPrefix + "CAMBRIAN_?"
	}
}

// noteEnvKey records that config key came from environment variable envVar.
// Called from the env provider's key-mapping callback, the only place both names
// are visible at once.
func (t *tracker) noteEnvKey(key, envVar string) {
	if t.envKeys == nil {
		t.envKeys = make(map[string]string)
	}
	t.envKeys[key] = envVar
}
