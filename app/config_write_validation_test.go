package app

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/storage"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// A runtime config write is checked against the same load-time rules boot runs
// (config.ValidateOverride), so a value the next boot would refuse is refused
// at write time instead of stored as a kernel that will not start
// (T-20261106-executionconfig-validate-has-no-production).
func TestSetConfig_RefusesValueTheLoadRulesReject(t *testing.T) {
	// The shipped catalogue's ranges currently sit inside the load rules, so a
	// catalogue entry with a deliberately wide range stands in for the case the
	// per-key range cannot see. Sequential test: safe to swap the package var.
	saved := tunables
	t.Cleanup(func() { tunables = saved })
	tunables = append(append([]tunable(nil), saved...), tunable{
		Key: "execution.cross_verify_rate", Min: -10, Max: 10,
	})

	dir := bundle(t, map[string]string{"embedder.json": `{"embedder": {"model": "e"}}`})
	store, err := storage.OpenConfigStore(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatalf("OpenConfigStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfgPath := filepath.Join(dir, "config.json")
	_, prov, err := config.LoadConfigWithStore(cfgPath, store)
	if err != nil {
		t.Fatalf("LoadConfigWithStore: %v", err)
	}
	src := configWriteSource{store: store, prov: prov, configPath: cfgPath}

	outs, err := src.SetConfig(t.Context(), map[string]float64{
		"execution.cross_verify_rate": 1.5, // in the catalogue range, outside [0, 1]
		"execution.ewma_alpha":        0.4, // valid: still written
	})
	if err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	bad := outcomeFor(t, outs, "execution.cross_verify_rate")
	if bad.Effect != operator.EffectRejected || bad.Set ||
		!strings.Contains(bad.Error, "execution.verification.cross_verify_rate must be in [0, 1]") {
		t.Fatalf("invalid write not refused with the key named: %+v", bad)
	}
	if good := outcomeFor(t, outs, "execution.ewma_alpha"); !good.Set {
		t.Fatalf("valid write in the same request was not stored: %+v", good)
	}

	// Nothing invalid reached the store: the next boot still loads.
	if _, _, err := config.LoadConfigWithStore(cfgPath, store); err != nil {
		t.Fatalf("store holds a value the next boot refuses: %v", err)
	}
}
