package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ADR-0101 D8 resolver + write-through, and D9 per-id generators.

const providersFixture = `{
  "llm_provider": {
    "default": "deepseek",
    "generators": [
      {"id": "deepseek", "provider": "openai", "model": "deepseek-v4-flash", "endpoint": "https://a", "api_key_env": "DEEPSEEK_KEY"},
      {"id": "mimo",     "provider": "openai", "model": "mimo-v2.5",         "endpoint": "https://b"}
    ],
    "roles": {"planner": "deepseek"}
  },
  "_comment": "kept verbatim"
}
`

func TestResolveFileSetting_HighestFileLayerWins(t *testing.T) {
	dir := baseBundle(t)
	writeCfg(t, dir, "providers.json", providersFixture)
	writeCfg(t, dir, "providers.local.json", `{"llm_provider": {"roles": {"planner": "mimo"}}}`)

	o, ok := ResolveFileSetting(filepath.Join(dir, "config.json"), "llm_provider.roles.planner")
	if !ok || o.Source != SourceProvidersLocal || o.Value != "mimo" || !o.Writable {
		t.Fatalf("want providers.local.json/mimo/writable, got ok=%v %+v", ok, o)
	}
	o, ok = ResolveFileSetting(filepath.Join(dir, "config.json"), "llm_provider.default")
	if !ok || o.Source != SourceProviders || o.Value != "deepseek" {
		t.Fatalf("want providers.json/deepseek, got ok=%v %+v", ok, o)
	}
	if _, ok := ResolveFileSetting(filepath.Join(dir, "config.json"), "llm_provider.roles.memory"); ok {
		t.Fatal("no file sets roles.memory; the resolver must say so")
	}
}

// The committed starter is never a write-through target, whatever its mode.
func TestResolveFileSetting_TuningJSONIsNotWritable(t *testing.T) {
	dir := baseBundle(t)
	writeCfg(t, dir, "tuning.json", `{"execution": {"ewma_alpha": 0.5}}`)

	o, ok := ResolveFileSetting(filepath.Join(dir, "config.json"), "execution.ewma_alpha")
	if !ok || o.Source != SourceTuning {
		t.Fatalf("flat key must resolve through its v2 path to tuning.json, got ok=%v %+v", ok, o)
	}
	if o.Writable || !strings.Contains(o.Reason, "committed") {
		t.Fatalf("tuning.json must be reported not writable (committed), got %+v", o)
	}
}

func TestResolveFileSetting_ReadOnlyFileIsNotWritable(t *testing.T) {
	dir := baseBundle(t)
	writeCfg(t, dir, "providers.json", providersFixture)
	p := filepath.Join(dir, "providers.json")
	if err := os.Chmod(p, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o600) })

	o, ok := ResolveFileSetting(filepath.Join(dir, "config.json"), "llm_provider.default")
	if !ok || o.Writable {
		t.Fatalf("a read-only providers.json must not be writable, got ok=%v %+v", ok, o)
	}
}

func TestWriteFileSetting_PreservesEverythingElse(t *testing.T) {
	dir := baseBundle(t)
	writeCfg(t, dir, "providers.json", providersFixture)
	cfgPath := filepath.Join(dir, "config.json")

	o, _ := ResolveFileSetting(cfgPath, "llm_provider.roles.planner")
	if err := WriteFileSetting(o, "llm_provider.roles.planner", "mimo"); err != nil {
		t.Fatalf("WriteFileSetting: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "providers.json"))
	want := strings.Replace(providersFixture, `"roles": {"planner": "deepseek"}`, `"roles": {"planner": "mimo"}`, 1)
	if string(got) != want {
		t.Fatalf("only the one value may change.\n--- got\n%s\n--- want\n%s", got, want)
	}
	// No temp file left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

// A v1 file keeps its flat spelling: the write must not add a second, nested copy.
func TestWriteFileSetting_LegacyFlatSpellingIsRewrittenInPlace(t *testing.T) {
	dir := baseBundle(t)
	writeCfg(t, dir, "tuning.local.json", "{\n  \"execution\": {\n    \"ewma_alpha\": 0.5\n  }\n}\n")
	cfgPath := filepath.Join(dir, "config.json")

	o, ok := ResolveFileSetting(cfgPath, "execution.ewma_alpha")
	if !ok || o.Source != SourceTuningLocal || !o.Writable {
		t.Fatalf("want writable tuning.local.json, got ok=%v %+v", ok, o)
	}
	if err := WriteFileSetting(o, "execution.ewma_alpha", 0.8); err != nil {
		t.Fatalf("WriteFileSetting: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "tuning.local.json"))
	if string(got) != "{\n  \"execution\": {\n    \"ewma_alpha\": 0.8\n  }\n}\n" {
		t.Fatalf("unexpected file:\n%s", got)
	}
	cfg, _ := loadFrom(t, dir, nil)
	if cfg.Execution.Supervision.EWMAAlpha != 0.8 {
		t.Fatalf("reload saw %v, want 0.8", cfg.Execution.Supervision.EWMAAlpha)
	}
}

func TestWriteFileSetting_RefusesSecretKeys(t *testing.T) {
	dir := baseBundle(t)
	o, ok := ResolveFileSetting(filepath.Join(dir, "config.json"), "database.password")
	if !ok {
		t.Fatal("config.json sets database.password")
	}
	if err := WriteFileSetting(o, "database.password", "leaked"); err == nil {
		t.Fatal("a secret must never be written into a file (ADR-0101 D5)")
	}
	got, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	if strings.Contains(string(got), "leaked") {
		t.Fatal("secret reached the file")
	}
}

func TestIsSecretKey(t *testing.T) {
	for key, want := range map[string]bool{
		"database.password":                    true,
		"telemetry.secret_key":                 true,
		"llm_provider.generators":              false,
		"mcp.servers":                          false,
		"x.api_key_env":                        false,
		"x.token_env":                          false,
		"x.api_key":                            true,
		"execution.supervision.ewma_alpha":     false,
		"llm_provider.generator_overrides.foo": false,
	} {
		if got := IsSecretKey(key); got != want {
			t.Errorf("IsSecretKey(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestWriteFileGenerator_ReplacesOnlyThatElement(t *testing.T) {
	dir := baseBundle(t)
	writeCfg(t, dir, "providers.json", providersFixture)
	cfgPath := filepath.Join(dir, "config.json")

	o, ok := ResolveGeneratorFile(cfgPath, "mimo")
	if !ok || o.Source != SourceProviders {
		t.Fatalf("want providers.json, got ok=%v %+v", ok, o)
	}
	edited := GeneratorConfig{ID: "mimo", Provider: "openai", Model: "mimo-v3", Endpoint: "https://b", TimeoutMs: 1000}
	if err := WriteFileGenerator(o, "mimo", edited, false); err != nil {
		t.Fatalf("WriteFileGenerator: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "providers.json"))
	s := string(got)
	// The other generator's line, and the unrelated key, are byte-identical.
	for _, keep := range []string{
		`{"id": "deepseek", "provider": "openai", "model": "deepseek-v4-flash", "endpoint": "https://a", "api_key_env": "DEEPSEEK_KEY"},`,
		`"_comment": "kept verbatim"`,
		`"roles": {"planner": "deepseek"}`,
	} {
		if !strings.Contains(s, keep) {
			t.Fatalf("lost %q:\n%s", keep, s)
		}
	}
	t.Logf("providers.json after the edit:\n%s", s)
	if !strings.Contains(s, `"model": "mimo-v3"`) || strings.Contains(s, "mimo-v2.5") {
		t.Fatalf("edit did not land:\n%s", s)
	}
	cfg, _ := loadFrom(t, dir, nil)
	if len(cfg.LLMProvider.Generators) != 2 || cfg.LLMProvider.Generators[1].Model != "mimo-v3" {
		t.Fatalf("reload: %+v", cfg.LLMProvider.Generators)
	}

	if err := WriteFileGenerator(o, "mimo", nil, true); err != nil {
		t.Fatalf("remove: %v", err)
	}
	cfg, _ = loadFrom(t, dir, nil)
	if len(cfg.LLMProvider.Generators) != 1 || cfg.LLMProvider.Generators[0].ID != "deepseek" {
		t.Fatalf("after remove: %+v", cfg.LLMProvider.Generators)
	}
}

func TestCommittedSourcesMatchGitignore(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	gi, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", ".gitignore"))
	if err != nil {
		t.Skipf("no .gitignore beside the module: %v", err)
	}
	for _, l := range FileLayers("configs/config.json") {
		ignored := strings.Contains(string(gi), "configs/"+filepath.Base(l.Path)+"\n") ||
			strings.Contains(string(gi), "configs/"+filepath.Base(l.Path)+"\r\n")
		if committedSources[l.Source] == ignored {
			t.Errorf("%s: committed=%v but gitignored=%v — committedSources must list exactly the tracked layer files",
				l.Source, committedSources[l.Source], ignored)
		}
	}
}

// ── D9 ───────────────────────────────────────────────────────────────────────

// The 2026-11-01 failure: a generator added to providers.json after a console
// edit of another generator must still be visible.
func TestStoredGenerator_MergesByIDOverTheFileList(t *testing.T) {
	dir := baseBundle(t)
	writeCfg(t, dir, "providers.json", providersFixture)
	writeCfg(t, dir, "embedder.json", `{"embedder": {"model": "e"}}`)
	store := mapStore{
		GeneratorOverrideKey("deepseek"): map[string]any{"id": "deepseek", "provider": "openai", "model": "edited"},
	}
	cfg, prov := loadFrom(t, dir, store)
	gens := cfg.LLMProvider.Generators
	if len(gens) != 2 || gens[0].Model != "edited" || gens[1].ID != "mimo" {
		t.Fatalf("stored generator must replace only its own id: %+v", gens)
	}
	if prov.Source(GeneratorsKey) != SourceStore {
		t.Fatalf("provenance = %q", prov.Source(GeneratorsKey))
	}

	// Add a generator to the FILE after the store edit: visible.
	writeCfg(t, dir, "providers.json", strings.Replace(providersFixture,
		`"endpoint": "https://b"}`, `"endpoint": "https://b"},
      {"id": "gemini", "provider": "gemini", "model": "gemini-3-flash", "endpoint": "https://c"}`, 1))
	cfg, _ = loadFrom(t, dir, store)
	if len(cfg.LLMProvider.Generators) != 3 || cfg.LLMProvider.Generators[2].ID != "gemini" {
		t.Fatalf("file-added generator hidden by a stored one: %+v", cfg.LLMProvider.Generators)
	}
}

func TestStoredGenerator_TombstoneHidesAFileGenerator(t *testing.T) {
	dir := baseBundle(t)
	writeCfg(t, dir, "providers.json", providersFixture)
	writeCfg(t, dir, "embedder.json", `{"embedder": {"model": "e"}}`)
	cfg, _ := loadFrom(t, dir, mapStore{
		GeneratorOverrideKey("mimo"): map[string]any{"id": "mimo", "removed": true},
	})
	if len(cfg.LLMProvider.Generators) != 1 || cfg.LLMProvider.Generators[0].ID != "deepseek" {
		t.Fatalf("tombstone must hide mimo: %+v", cfg.LLMProvider.Generators)
	}
}

// A store written before D9 holds the whole list. Load merges it per id in
// memory; MigrateStoredGenerators persists per-id entries and drops the list.
func TestStoredGenerator_LegacyWholeListMigrates(t *testing.T) {
	dir := baseBundle(t)
	writeCfg(t, dir, "providers.json", strings.Replace(providersFixture,
		`"endpoint": "https://b"}`, `"endpoint": "https://b"},
      {"id": "gemini", "provider": "gemini", "model": "gemini-3-flash", "endpoint": "https://c"}`, 1))
	writeCfg(t, dir, "embedder.json", `{"embedder": {"model": "e"}}`)
	legacy := []any{
		map[string]any{"id": "deepseek", "provider": "openai", "model": "stored-ds"},
		map[string]any{"id": "mimo", "provider": "openai", "model": "stored-mimo"},
	}
	store := mapStore{GeneratorsKey: legacy, "llm_provider.default": "deepseek"}

	// In memory: gemini (file only) is visible; stored entries win by id.
	cfg, _ := loadFrom(t, dir, store)
	if len(cfg.LLMProvider.Generators) != 3 || cfg.LLMProvider.Generators[0].Model != "stored-ds" ||
		cfg.LLMProvider.Generators[2].ID != "gemini" {
		t.Fatalf("legacy list not merged per id: %+v", cfg.LLMProvider.Generators)
	}

	ids, err := MigrateStoredGenerators(store)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if strings.Join(ids, ",") != "deepseek,mimo" {
		t.Fatalf("migrated ids = %v", ids)
	}
	if _, still := store[GeneratorsKey]; still {
		t.Fatal("the legacy list key must be deleted")
	}
	if _, ok := store[GeneratorOverrideKey("mimo")]; !ok {
		t.Fatal("per-id entry missing after migration")
	}
	cfg2, _ := loadFrom(t, dir, store)
	if len(cfg2.LLMProvider.Generators) != 3 || cfg2.LLMProvider.Generators[1].Model != "stored-mimo" {
		t.Fatalf("after migration: %+v", cfg2.LLMProvider.Generators)
	}
	// Idempotent.
	if ids, err := MigrateStoredGenerators(store); err != nil || len(ids) != 0 {
		t.Fatalf("second migrate = %v, %v; want no-op", ids, err)
	}
	// Default validation still applies: a default naming an id nobody declares fails.
	store["llm_provider.default"] = "nope"
	if _, _, err := LoadConfigWithStore(filepath.Join(dir, "config.json"), store); err == nil {
		t.Fatal("an undeclared default must still fail validation")
	}
}

func TestStoreOverrideReport_DiffersOnlyForDifferentValues(t *testing.T) {
	dir := baseBundle(t)
	writeCfg(t, dir, "providers.json", providersFixture)
	cfgPath := filepath.Join(dir, "config.json")
	rep := StoreOverrideReport(cfgPath, map[string]any{
		"llm_provider.roles.planner":     "mimo",     // differs from providers.json
		"llm_provider.default":           "deepseek", // equal
		"llm_provider.roles.memory":      "mimo",     // no file sets it
		GeneratorOverrideKey("deepseek"): map[string]any{"id": "deepseek", "provider": "openai", "model": "x"},
	})
	byKey := map[string]StoreOverride{}
	for _, r := range rep {
		byKey[r.Key] = r
	}
	if r := byKey["llm_provider.roles.planner"]; !r.Differs || r.File != SourceProviders || r.FileValue != "deepseek" {
		t.Fatalf("planner: %+v", r)
	}
	if r := byKey["llm_provider.default"]; r.Differs || r.File != SourceProviders {
		t.Fatalf("default: %+v", r)
	}
	if r := byKey["llm_provider.roles.memory"]; r.Differs || r.File != "" {
		t.Fatalf("memory: %+v", r)
	}
	if r := byKey[GeneratorOverrideKey("deepseek")]; !r.Differs || r.File != SourceProviders {
		t.Fatalf("generator: %+v", r)
	}
}
