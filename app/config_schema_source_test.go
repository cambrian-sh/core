package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
)

func bundle(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files["config.json"] = `{"database":{"host":"h","user":"u","password":"p","dbname":"d"}}`
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func sourceFor(t *testing.T, dir string) configSchemaSource {
	t.Helper()
	cfg, prov, err := config.LoadConfigWithStore(filepath.Join(dir, "config.json"), nil)
	if err != nil {
		t.Fatalf("LoadConfigWithStore: %v", err)
	}
	return configSchemaSource{cfg: cfg, prov: prov}
}

// The end-to-end property GetConfigSchema exists for: when an env var pins a
// key, the console must be able to NAME it.
//
// Without this, an operator changes a value, sees "saved", nothing happens, and
// there is nothing anywhere in the product that explains why.
func TestConfigSchema_ValueSourceNamesThePinningEnvVar(t *testing.T) {
	dir := bundle(t, map[string]string{
		"tuning.json": `{"execution":{"ewma_alpha":0.5}}`,
	})
	t.Setenv("CAMBRIAN_EXECUTION__EWMA_ALPHA", "0.91")

	src := sourceFor(t, dir)
	got := src.ValueSource()["execution.ewma_alpha"]

	want := config.EnvSource("CAMBRIAN_EXECUTION__EWMA_ALPHA")
	if got != want {
		t.Fatalf("value_source = %q, want %q", got, want)
	}
	// And the reported value must be the one the env var forced, not the file's.
	if v := src.CurrentValues()["execution.ewma_alpha"]; v != 0.91 {
		t.Fatalf("current value = %v, want 0.91 — reporting the file's value would be a lie", v)
	}
}

// A file-pinned key must name the file, so the operator knows what to edit.
func TestConfigSchema_ValueSourceNamesThePinningFile(t *testing.T) {
	dir := bundle(t, map[string]string{
		"tuning.json":       `{"execution":{"ewma_alpha":0.5}}`,
		"tuning.local.json": `{"execution":{"ewma_alpha":0.6}}`,
	})

	src := sourceFor(t, dir)
	if got := src.ValueSource()["execution.ewma_alpha"]; got != config.SourceTuningLocal {
		t.Fatalf("value_source = %q, want %q", got, config.SourceTuningLocal)
	}
}

// An untouched key reports "default", never an absent entry. An absent entry
// renders as "unknown", and "unknown" on every untouched field would drown the
// handful of genuine pins that matter.
func TestConfigSchema_UntouchedKeyReportsDefaultNotUnknown(t *testing.T) {
	src := sourceFor(t, bundle(t, map[string]string{}))
	sources := src.ValueSource()

	for _, tn := range tunables {
		if sources[tn.Key] == "" {
			t.Fatalf("%s has no value_source entry — the console would render it as unknown", tn.Key)
		}
	}
}

// Every catalogued key must exist in the provenance map, which is the check that
// catches a typo'd key. A key the tracker has never seen reports "default"
// forever and looks exactly like a working field.
func TestConfigSchema_EveryCataloguedKeyIsARealConfigKey(t *testing.T) {
	dir := bundle(t, map[string]string{})
	cfg, prov, err := config.LoadConfigWithStore(filepath.Join(dir, "config.json"), nil)
	if err != nil {
		t.Fatalf("LoadConfigWithStore: %v", err)
	}

	for _, tn := range tunables {
		if tn.Runtime {
			continue // no config-file key by construction; see tunable.Runtime
		}
		if prov.Source(tn.Key) == "" {
			// One legitimate exception: a real config field tagged `omitempty`
			// whose value is the zero value marshals away entirely, so no layer
			// states it. Confirm it is at least a field the merged config carries.
			if _, ok := (configSchemaSource{cfg: cfg}).bootValue(tn.Key); !ok {
				t.Errorf("tunable %q is neither a runtime key nor a real config key — value_source would silently report nothing for it", tn.Key)
			}
		}
	}
}

// Writable and read-only keys must partition the catalogue: a key in neither list
// is invisible to the form, and one in both is a contradiction.
func TestConfigSchema_KeysPartitionIntoEditableAndKernelOnly(t *testing.T) {
	src := sourceFor(t, bundle(t, map[string]string{}))

	editable := map[string]bool{}
	for _, k := range src.EditableKeys() {
		editable[k] = true
	}
	kernelOnly := map[string]bool{}
	for _, k := range src.KernelOnlyKeys() {
		if editable[k] {
			t.Fatalf("%q is both editable and kernel-only", k)
		}
		kernelOnly[k] = true
	}
	for _, tn := range tunables {
		if !editable[tn.Key] && !kernelOnly[tn.Key] {
			t.Fatalf("%q appears in neither list — the form would not render it at all", tn.Key)
		}
	}
}

// A hot-applied value must win over the booted one: it is what the kernel is
// using right now, which is the question the form asks.
func TestConfigSchema_LiveValueBeatsBootedValue(t *testing.T) {
	src := sourceFor(t, bundle(t, map[string]string{}))
	src.live = func(param string) (float64, bool) {
		if param == "blend_weight_cosine" {
			return 0.77, true
		}
		return 0, false
	}

	if v := src.CurrentValues()["execution.blend_weight_cosine"]; v != 0.77 {
		t.Fatalf("current value = %v, want the hot-applied 0.77", v)
	}
}

func TestConfigSchema_SchemaIsStableAndHashed(t *testing.T) {
	src := sourceFor(t, bundle(t, map[string]string{}))

	s1, v1, h1 := src.SchemaJSON()
	_, _, h2 := src.SchemaJSON()
	if s1 == "" || v1 == "" || h1 == "" {
		t.Fatalf("empty schema/version/hash: %q %q %q", s1, v1, h1)
	}
	if h1 != h2 {
		t.Fatalf("hash unstable across calls: %q vs %q — a console caching by hash would refetch forever", h1, h2)
	}
}
