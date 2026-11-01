package app

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/storage"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// ADR-0101 D8/D9 through the real write path (configWriteSource) and a real
// bbolt store, against temp config bundles only.

const d8Providers = `{
  "llm_provider": {
    "default": "deepseek",
    "generators": [
      {"id": "deepseek", "provider": "openai", "model": "deepseek-v4-flash", "endpoint": "https://a", "api_key_env": "D8_TEST_DEEPSEEK_KEY", "cost_per_1m_input": 0.3, "cost_per_1m_output": 1.2},
      {"id": "mimo", "provider": "openai", "model": "mimo-v2.5", "endpoint": "https://b"}
    ],
    "roles": {"planner": "deepseek"}
  }
}
`

// d8Source builds a write source over a temp bundle with write-through enabled.
func d8Source(t *testing.T, files map[string]string) (configWriteSource, string) {
	t.Helper()
	files["embedder.json"] = `{"embedder": {"model": "e"}}`
	dir := bundle(t, files)
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
	return configWriteSource{store: store, prov: prov, configPath: cfgPath}, dir
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A role a writable providers.local.json sets goes into THAT file (the highest
// layer), not providers.json and not the store; a stale store override is removed.
func TestD8_RoleWriteGoesToTheHighestWritableFileAndClearsTheStore(t *testing.T) {
	local := `{"llm_provider": {"roles": {"planner": "deepseek"}}, "keep": [1, 2, 3]}`
	src, dir := d8Source(t, map[string]string{"providers.json": d8Providers, "providers.local.json": local})
	// A pre-D8 console write left an override behind.
	if err := src.store.SetOverride("llm_provider.roles.planner", "deepseek"); err != nil {
		t.Fatal(err)
	}

	out, err := src.SetRoleAssignment("planner", "mimo")
	if err != nil || !out.Set || out.WrittenTo != config.SourceProvidersLocal {
		t.Fatalf("want written to providers.local.json, got %+v err=%v", out, err)
	}
	if got := readFile(t, filepath.Join(dir, "providers.local.json")); got != strings.Replace(local, `"planner": "deepseek"`, `"planner": "mimo"`, 1) {
		t.Fatalf("providers.local.json: only the role may change, got %s", got)
	}
	if got := readFile(t, filepath.Join(dir, "providers.json")); got != d8Providers {
		t.Fatal("providers.json (the lower layer) must not be touched")
	}
	ov, _ := src.store.Overrides()
	if _, ok := ov["llm_provider.roles.planner"]; ok {
		t.Fatal("the store override must be removed after write-through")
	}
	cfg, _, err := config.LoadConfigWithStore(src.configPath, src.store)
	if err != nil || cfg.LLMProvider.Roles["planner"] != "mimo" {
		t.Fatalf("reload: roles=%v err=%v", cfg.LLMProvider.Roles, err)
	}
}

// No file sets the key: stored, as before D8.
func TestD8_KeyNoFileSetsIsStored(t *testing.T) {
	src, dir := d8Source(t, map[string]string{"providers.json": d8Providers})
	out, err := src.SetRoleAssignment("memory", "mimo")
	if err != nil || !out.Set || out.WrittenTo != "" || out.Overrides != "" {
		t.Fatalf("want a plain store write, got %+v err=%v", out, err)
	}
	ov, _ := src.store.Overrides()
	if ov["llm_provider.roles.memory"] != "mimo" {
		t.Fatalf("store: %v", ov)
	}
	if got := readFile(t, filepath.Join(dir, "providers.json")); got != d8Providers {
		t.Fatal("providers.json must not change when it does not set the key")
	}
}

// A read-only file that sets the key: stored, and the ack names the file and
// the value the store now beats.
func TestD8_ReadOnlyFileStoresAndReportsTheOverride(t *testing.T) {
	src, dir := d8Source(t, map[string]string{"providers.json": d8Providers})
	p := filepath.Join(dir, "providers.json")
	if err := os.Chmod(p, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o600) })

	out, err := src.SetRoleAssignment("planner", "mimo")
	if err != nil || !out.Set {
		t.Fatalf("SetRoleAssignment: %+v err=%v", out, err)
	}
	if out.WrittenTo != "" || out.Overrides != config.SourceProviders || out.OverridesValue != "deepseek" {
		t.Fatalf("want overrides providers.json (deepseek), got %+v", out)
	}
	ov, _ := src.store.Overrides()
	if ov["llm_provider.roles.planner"] != "mimo" {
		t.Fatalf("store must hold the value: %v", ov)
	}
	if readFile(t, p) != d8Providers {
		t.Fatal("a read-only file must not change")
	}
}

// The committed tuning.json is never rewritten; a tuning.local.json that sets
// the key is.
func TestD8_TrackedTuningJSONIsNeverRewritten(t *testing.T) {
	tuning := `{"execution": {"ewma_alpha": 0.5}}`
	src, dir := d8Source(t, map[string]string{"tuning.json": tuning})

	outs, err := src.SetConfig(map[string]float64{"execution.ewma_alpha": 0.8})
	if err != nil {
		t.Fatal(err)
	}
	o := outcomeFor(t, outs, "execution.ewma_alpha")
	if !o.Set || o.WrittenTo != "" || o.Overrides != config.SourceTuning || o.OverridesValue != "0.5" {
		t.Fatalf("want stored + overrides tuning.json (0.5), got %+v", o)
	}
	if readFile(t, filepath.Join(dir, "tuning.json")) != tuning {
		t.Fatal("tuning.json is committed and must never be rewritten")
	}

	// Now a per-machine file sets it: the write lands there.
	local := "{\n  \"execution\": {\n    \"supervision\": {\n      \"ewma_alpha\": 0.6\n    }\n  }\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "tuning.local.json"), []byte(local), 0o600); err != nil {
		t.Fatal(err)
	}
	outs, _ = src.SetConfig(map[string]float64{"execution.ewma_alpha": 0.7})
	o = outcomeFor(t, outs, "execution.ewma_alpha")
	if o.WrittenTo != config.SourceTuningLocal {
		t.Fatalf("want written to tuning.local.json, got %+v", o)
	}
	if got := readFile(t, filepath.Join(dir, "tuning.local.json")); got != strings.Replace(local, "0.6", "0.7", 1) {
		t.Fatalf("tuning.local.json:\n%s", got)
	}
	ov, _ := src.store.Overrides()
	if _, ok := ov["execution.ewma_alpha"]; ok {
		t.Fatal("the earlier store override must be removed once the value lives in a file")
	}
	if readFile(t, filepath.Join(dir, "tuning.json")) != tuning {
		t.Fatal("tuning.json changed")
	}
}

// Env still wins (D1/D3): a write-through to a file is still reported shadowed.
func TestD8_EnvStillShadowsAWriteThrough(t *testing.T) {
	local := `{"execution": {"ewma_alpha": 0.6}}`
	t.Setenv("CAMBRIAN_EXECUTION__EWMA_ALPHA", "0.42")
	src, _ := d8Source(t, map[string]string{"tuning.local.json": local})
	outs, _ := src.SetConfig(map[string]float64{"execution.ewma_alpha": 0.7})
	o := outcomeFor(t, outs, "execution.ewma_alpha")
	if o.Effect != operator.EffectShadowed || o.ShadowedBy != "env:CAMBRIAN_EXECUTION__EWMA_ALPHA" || o.WrittenTo != config.SourceTuningLocal {
		t.Fatalf("want shadowed by env, written to tuning.local.json; got %+v", o)
	}
}

// Secrets never reach a file (D5), even when a file declares the generator.
func TestD8_SecretsAreNeverWrittenToAFile(t *testing.T) {
	src, dir := d8Source(t, map[string]string{"providers.json": d8Providers})
	if _, err := src.SetGeneratorKey("mimo", "sk-very-secret-value"); err != nil {
		t.Fatal(err)
	}
	if !src.store.Configured("generator:mimo:api_key") {
		t.Fatal("the key must be in the secret store")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(readFile(t, filepath.Join(dir, e.Name())), "sk-very-secret-value") {
			t.Fatalf("secret leaked into %s", e.Name())
		}
	}
}

// ── D9 + D8: generators ──────────────────────────────────────────────────────

// Editing a file-declared generator writes that element into providers.json
// (keeping catalogue cost and api_key_env), and nothing lands in the store.
func TestD8_SaveGeneratorEditsTheFileElement(t *testing.T) {
	src, dir := d8Source(t, map[string]string{"providers.json": d8Providers})
	out, err := src.SaveGenerator(operator.GeneratorSpec{ID: "deepseek", Provider: "openai", Model: "deepseek-v5", Endpoint: "https://a"})
	if err != nil || !out.Set || out.WrittenTo != config.SourceProviders {
		t.Fatalf("want written to providers.json, got %+v err=%v", out, err)
	}
	got := readFile(t, filepath.Join(dir, "providers.json"))
	if !strings.Contains(got, `{"id": "mimo", "provider": "openai", "model": "mimo-v2.5", "endpoint": "https://b"}`) {
		t.Fatalf("the other generator must be byte-identical:\n%s", got)
	}
	ov, _ := src.store.Overrides()
	if len(ov) != 0 {
		t.Fatalf("store must stay empty, got %v", ov)
	}
	cfg, _, err := config.LoadConfigWithStore(src.configPath, src.store)
	if err != nil {
		t.Fatal(err)
	}
	g := cfg.LLMProvider.Generators[0]
	if g.Model != "deepseek-v5" || g.CostPer1MInput != 0.3 || g.APIKeyEnv != "D8_TEST_DEEPSEEK_KEY" {
		t.Fatalf("edit lost catalogue fields: %+v", g)
	}
}

// The 2026-11-01 failure, end to end: with providers.json read-only, a console
// edit of one generator is stored per id — and a generator later added to the
// file is still visible, and can become the default.
func TestD9_FileAddedGeneratorStaysVisibleAfterAStoredEdit(t *testing.T) {
	src, dir := d8Source(t, map[string]string{"providers.json": d8Providers})
	p := filepath.Join(dir, "providers.json")
	_ = os.Chmod(p, 0o444)

	out, err := src.SaveGenerator(operator.GeneratorSpec{ID: "mimo", Provider: "openai", Model: "mimo-v3", Endpoint: "https://b"})
	if err != nil || out.Overrides != config.SourceProviders {
		t.Fatalf("want stored + overrides providers.json, got %+v err=%v", out, err)
	}
	ov, _ := src.store.Overrides()
	if _, ok := ov[config.GeneratorOverrideKey("mimo")]; !ok {
		t.Fatalf("want a per-id store entry, got %v", ov)
	}
	if _, ok := ov[config.GeneratorsKey]; ok {
		t.Fatal("the whole list must never be stored any more")
	}

	_ = os.Chmod(p, 0o600)
	added := strings.Replace(d8Providers, `"endpoint": "https://b"}`, `"endpoint": "https://b"},
      {"id": "gemini", "provider": "gemini", "model": "gemini-3-flash", "endpoint": "https://c"}`, 1)
	added = strings.Replace(added, `"default": "deepseek"`, `"default": "gemini"`, 1)
	if err := os.WriteFile(p, []byte(added), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.LoadConfigWithStore(src.configPath, src.store)
	if err != nil {
		t.Fatalf("a file-added default must boot: %v", err)
	}
	ids := []string{}
	for _, g := range cfg.LLMProvider.Generators {
		ids = append(ids, g.ID+"="+g.Model)
	}
	if strings.Join(ids, ",") != "deepseek=deepseek-v4-flash,mimo=mimo-v3,gemini=gemini-3-flash" {
		t.Fatalf("generators = %v", ids)
	}
}

// A store holding the pre-D9 list is migrated by the first generator write.
func TestD9_GeneratorWriteMigratesTheLegacyList(t *testing.T) {
	src, _ := d8Source(t, map[string]string{})
	legacy := []config.GeneratorConfig{{ID: "a", Provider: "openai", Model: "m"}, {ID: "b", Provider: "openai", Model: "m"}}
	_ = src.store.SetOverride(config.GeneratorsKey, legacy)
	_ = src.store.SetOverride("llm_provider.default", "a")

	if _, err := src.SaveGenerator(operator.GeneratorSpec{ID: "c", Provider: "openai", Model: "m"}); err != nil {
		t.Fatal(err)
	}
	ov, _ := src.store.Overrides()
	if _, ok := ov[config.GeneratorsKey]; ok {
		t.Fatal("legacy list key must be gone")
	}
	for _, id := range []string{"a", "b", "c"} {
		if _, ok := ov[config.GeneratorOverrideKey(id)]; !ok {
			t.Fatalf("missing per-id entry %s: %v", id, ov)
		}
	}
	// Default/role guards still judge the effective list.
	if o, _ := src.RemoveGenerator("a"); o.Effect != operator.EffectRejected {
		t.Fatalf("removing the default must be refused, got %+v", o)
	}
}

// Removing a generator from a read-only file leaves a tombstone.
func TestD9_RemoveFromAReadOnlyFileTombstones(t *testing.T) {
	src, dir := d8Source(t, map[string]string{"providers.json": d8Providers})
	p := filepath.Join(dir, "providers.json")
	_ = os.Chmod(p, 0o444)
	t.Cleanup(func() { _ = os.Chmod(p, 0o600) })

	out, err := src.RemoveGenerator("mimo")
	if err != nil || !out.Set || out.Overrides != config.SourceProviders {
		t.Fatalf("want stored tombstone overriding providers.json, got %+v err=%v", out, err)
	}
	cfg, _, err := config.LoadConfigWithStore(src.configPath, src.store)
	if err != nil || len(cfg.LLMProvider.Generators) != 1 {
		t.Fatalf("mimo must be hidden: %+v err=%v", cfg.LLMProvider.Generators, err)
	}
}

// ── boot WARN ────────────────────────────────────────────────────────────────

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestD8_BootWarnsOnlyForADifferingOverride(t *testing.T) {
	src, _ := d8Source(t, map[string]string{"providers.json": d8Providers})
	_ = src.store.SetOverride("llm_provider.roles.planner", "mimo")  // differs
	_ = src.store.SetOverride("llm_provider.default", "deepseek")    // equal
	_ = src.store.SetOverride("database.password", "store-password") // secret, differs

	logs := captureLogs(t)
	warnStoreOverrides(src.configPath, src.store)
	out := logs.String()

	if !strings.Contains(out, "key=llm_provider.roles.planner") || !strings.Contains(out, "file_value=deepseek") ||
		!strings.Contains(out, "store_value=mimo") || !strings.Contains(out, "providers.json") {
		t.Fatalf("missing WARN for the differing role:\n%s", out)
	}
	if strings.Contains(out, "key=llm_provider.default") {
		t.Fatalf("an override equal to the file value must not warn:\n%s", out)
	}
	if strings.Contains(out, "store-password") || strings.Contains(out, "file_value=p ") {
		t.Fatalf("secret values must never be logged:\n%s", out)
	}
	if !strings.Contains(out, "key=database.password") || !strings.Contains(out, "<redacted>") {
		t.Fatalf("the secret override itself must still be reported, redacted:\n%s", out)
	}
}

// ── CLI ──────────────────────────────────────────────────────────────────────

func cliBundle(t *testing.T) (dir string) {
	t.Helper()
	dir = t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "configs"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"config.json":    `{"database":{"host":"h","user":"u","password":"p","dbname":"d"}}`,
		"providers.json": d8Providers,
	} {
		if err := os.WriteFile(filepath.Join(dir, "configs", name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(ConfigStoreEnv, "")
	return dir
}

func TestConfigCLI_ListAndUnset(t *testing.T) {
	dir := cliBundle(t)
	store, err := storage.OpenConfigStore(filepath.Join(dir, "configs", "config.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = store.SetOverride("llm_provider.roles.planner", "mimo")
	_ = store.SetOverride("llm_provider.roles.memory", "mimo")
	_ = store.Close()

	var out, errb bytes.Buffer
	if code := runConfig([]string{"overrides", "list", "--dir", dir}, &out, &errb); code != 0 {
		t.Fatalf("list exit %d: %s", code, errb.String())
	}
	s := out.String()
	if !strings.Contains(s, "llm_provider.roles.planner = mimo") || !strings.Contains(s, "OVERRIDES providers.json = deepseek") ||
		!strings.Contains(s, "llm_provider.roles.memory = mimo") || !strings.Contains(s, "no config file sets this key") {
		t.Fatalf("list output:\n%s", s)
	}

	out.Reset()
	errb.Reset()
	if code := runConfig([]string{"overrides", "unset", "llm_provider.roles.planner", "--dir", dir, "no.such.key"}, &out, &errb); code != 1 {
		t.Fatalf("unset with one unknown key must exit 1, got %d", code)
	}
	if !strings.Contains(errb.String(), `no override "no.such.key"`) || !strings.Contains(out.String(), "unset llm_provider.roles.planner") {
		t.Fatalf("unset output: %s / %s", out.String(), errb.String())
	}

	store, err = storage.OpenConfigStore(filepath.Join(dir, "configs", "config.db"))
	if err != nil {
		t.Fatal(err)
	}
	ov, _ := store.Overrides()
	_ = store.Close()
	if _, ok := ov["llm_provider.roles.planner"]; ok || ov["llm_provider.roles.memory"] != "mimo" {
		t.Fatalf("store after unset: %v", ov)
	}
}

func TestConfigCLI_RefusesWhileTheKernelHoldsTheLock(t *testing.T) {
	dir := cliBundle(t)
	held, err := storage.OpenConfigStore(filepath.Join(dir, "configs", "config.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()

	for _, args := range [][]string{
		{"overrides", "list", "--dir", dir},
		{"overrides", "unset", "x", "--dir", dir},
	} {
		var out, errb bytes.Buffer
		if code := runConfig(args, &out, &errb); code != 1 || !strings.Contains(errb.String(), "kernel is probably running") {
			t.Fatalf("%v: exit %d, stderr %q", args, code, errb.String())
		}
	}
}

func TestConfigCLI_ListWithNoStoreCreatesNothing(t *testing.T) {
	dir := cliBundle(t)
	var out, errb bytes.Buffer
	if code := runConfig([]string{"overrides", "list", "--dir", dir}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	for _, f := range []string{"config.db", "secret.key"} {
		if _, err := os.Stat(filepath.Join(dir, "configs", f)); err == nil {
			t.Fatalf("list must not create %s", f)
		}
	}
}

// MCP servers keep their whole-list key (D9 is generators only), but D8 still
// applies: with mcp.json setting mcp.servers, saves land in mcp.json and two
// saves in one process compose through the file.
func TestD8_MCPSavesGoToMCPJSONAndCompose(t *testing.T) {
	mcpJSON := `{"mcp": {"servers": [{"id": "fs", "transport": "stdio", "endpoint": "fs-server"}]}, "other": true}`
	src, dir := d8Source(t, map[string]string{"mcp.json": mcpJSON})
	for _, id := range []string{"web", "git"} {
		out, err := src.SaveMCPServer(operator.MCPServerSpec{ID: id, Transport: "http", Endpoint: "http://" + id})
		if err != nil || out.WrittenTo != config.SourceMCP {
			t.Fatalf("save %s: %+v err=%v", id, out, err)
		}
	}
	got := readFile(t, filepath.Join(dir, "mcp.json"))
	if !strings.Contains(got, `"other": true`) {
		t.Fatalf("unrelated content lost:\n%s", got)
	}
	cfg, _, err := config.LoadConfigWithStore(src.configPath, src.store)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(cfg.MCP.Servers); n != 3 {
		t.Fatalf("want fs, web, git; got %d: %+v", n, cfg.MCP.Servers)
	}
	if ov, _ := src.store.Overrides(); len(ov) != 0 {
		t.Fatalf("store must stay empty: %v", ov)
	}
}
