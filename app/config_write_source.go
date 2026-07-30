package app

import (
	"context"
	"fmt"
	"math"
	"os"
	"sort"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/storage"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// configWriteSource implements operator.ConfigWriter and operator.SecretWriter
// against the ADR-0101 store.
type configWriteSource struct {
	store *storage.BoltConfigStore
	// prov is the boot provenance. It answers "is a layer ABOVE the store
	// supplying this key?", which is how a shadowed write is detected at write
	// time rather than left for a later read (ADR-0101 D3).
	prov config.Provenance
	// hotApply applies a value to the running kernel. Returns false when the key
	// has no live path, which makes the write restart-required rather than
	// failed — the value IS stored either way.
	hotApply func(param string, v float64) bool
	// generators reports the configured generator ids, so a credential cannot be
	// stored against a generator that does not exist.
	generators func() map[string]string // id -> APIKeyEnv
}

// tunableByKey indexes the catalogue for validation.
func tunableByKey(key string) (tunable, bool) {
	for _, t := range tunables {
		if t.Key == key {
			return t, true
		}
	}
	return tunable{}, false
}

// SetConfig validates, stores, and where possible hot-applies each key.
//
// Order matters and is deliberate: VALIDATE, then STORE, then apply. Storing
// before applying means a value that is valid but whose live application fails
// still persists and takes effect on the next boot — the operator's intent is
// recorded either way. The reverse order could apply a change that then fails to
// persist, which is the state that surprises someone after a restart.
func (c configWriteSource) SetConfig(values map[string]float64) ([]operator.ConfigWriteOutcome, error) {
	if c.store == nil {
		return nil, fmt.Errorf("no configuration store is available")
	}

	// Sorted so the response order is stable and two identical requests produce
	// identical responses — a console diffing them should see no churn.
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]operator.ConfigWriteOutcome, 0, len(keys))
	for _, key := range keys {
		out = append(out, c.setOne(key, values[key]))
	}
	return out, nil
}

func (c configWriteSource) setOne(key string, v float64) operator.ConfigWriteOutcome {
	res := operator.ConfigWriteOutcome{Key: key}

	t, known := tunableByKey(key)
	if !known {
		// Skip-and-warn, not a whole-request failure. A console built against a
		// different kernel revision must not lose the writes that WERE valid, and
		// a retired axis must not be writable back in by accident.
		res.Effect = operator.EffectRejected
		res.Error = "unknown configuration key on this kernel"
		return res
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		res.Effect = operator.EffectRejected
		res.Error = "value must be a finite number"
		return res
	}
	if v < t.Min || v > t.Max {
		res.Effect = operator.EffectRejected
		res.Error = fmt.Sprintf("value %g is outside the permitted range [%g, %g]", v, t.Min, t.Max)
		return res
	}

	if err := c.store.SetOverride(key, v); err != nil {
		res.Effect = operator.EffectRejected
		res.Error = "could not write to the configuration store: " + err.Error()
		return res
	}
	res.Set = true

	// ADR-0101 D3: a higher layer wins over the store. The value is STORED — it
	// is the operator's stated intent and takes effect the moment the shadowing
	// variable is removed — but nothing changes now, and saying so here is the
	// entire point. Left to a later read, the operator sees "saved", observes no
	// change, and has nothing to explain it.
	if pin := c.prov.PinnedAbove(key); pin != "" {
		res.Effect = operator.EffectShadowed
		res.ShadowedBy = pin
		return res
	}

	if t.Param != "" && c.hotApply != nil && c.hotApply(t.Param, v) {
		res.Effect = operator.EffectLive
		return res
	}
	res.Effect = operator.EffectRestartRequired
	return res
}

// DeleteConfig removes stored overrides.
//
// Deleting a key the store never held is NOT an error: the post-condition the
// caller wants — "the store does not pin this" — already holds, and reporting a
// failure would train an operator to ignore the response.
func (c configWriteSource) DeleteConfig(keys []string) ([]operator.ConfigWriteOutcome, error) {
	if c.store == nil {
		return nil, fmt.Errorf("no configuration store is available")
	}
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)

	out := make([]operator.ConfigWriteOutcome, 0, len(sorted))
	for _, key := range sorted {
		res := operator.ConfigWriteOutcome{Key: key}
		if err := c.store.DeleteOverride(key); err != nil {
			res.Effect = operator.EffectRejected
			res.Error = "could not write to the configuration store: " + err.Error()
			out = append(out, res)
			continue
		}
		res.Set = true
		// Always restart-required, even for a key with a live path. Reverting means
		// "use whatever the layers beneath supply", and the kernel only re-reads
		// those at boot — claiming "live" here would assert the running value had
		// changed back when it has not.
		res.Effect = operator.EffectRestartRequired
		out = append(out, res)
	}
	return out, nil
}

// SetGeneratorKey stores a provider credential.
func (c configWriteSource) SetGeneratorKey(generatorID, key string) (operator.ConfigWriteOutcome, error) {
	res := operator.ConfigWriteOutcome{Key: "generator:" + generatorID + ":api_key"}
	if c.store == nil {
		return res, fmt.Errorf("no credential store is available")
	}

	var envVar string
	if c.generators != nil {
		gens := c.generators()
		var known bool
		envVar, known = gens[generatorID]
		if !known {
			// Refused rather than stored. A credential filed against a generator
			// that does not exist is invisible: it never gets used, nothing errors,
			// and the operator believes the provider is configured.
			res.Effect = operator.EffectRejected
			res.Error = fmt.Sprintf("no generator with id %q is configured", generatorID)
			return res, nil
		}
	}

	if err := c.store.SetSecret(res.Key, key); err != nil {
		res.Effect = operator.EffectRejected
		res.Error = "could not write to the credential store: " + err.Error()
		return res, nil
	}
	res.Set = true

	// Same shadow rule as config, and it bites harder here: an operator who pastes
	// a new key while CAMBRIAN's env var still holds the old one would otherwise
	// see "saved" and keep getting authentication failures from the old credential.
	if envVar != "" && os.Getenv(envVar) != "" {
		res.Effect = operator.EffectShadowed
		res.ShadowedBy = "env:" + envVar
		return res, nil
	}

	// Credentials are resolved per call, so a stored key is live immediately —
	// no restart, and no process-wide cache to invalidate.
	res.Effect = operator.EffectLive
	return res, nil
}

// ClearGeneratorKey removes a stored credential.
func (c configWriteSource) ClearGeneratorKey(generatorID string) error {
	if c.store == nil {
		return fmt.Errorf("no credential store is available")
	}
	return c.store.ClearSecret("generator:" + generatorID + ":api_key")
}

// hotApplyFor builds the live-apply hook from the existing ADR-0054 seam, so
// there is ONE code path that mutates live tunables rather than two that can
// drift.
func hotApplyFor(effects interface {
	SetRuntimeConfig(ctx context.Context, params map[string]float64) error
}) func(string, float64) bool {
	if effects == nil {
		return nil
	}
	return func(param string, v float64) bool {
		return effects.SetRuntimeConfig(context.Background(), map[string]float64{param: v}) == nil
	}
}
