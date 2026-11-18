package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
)

// ENG-2121 end to end. The readiness run saved configs/providers.json with a
// UTF-8 BOM (Windows PowerShell 5 `Set-Content -Encoding utf8`); the kernel
// skipped the whole providers layer in silence, booted with no LLM and answered
// /healthz 200. Owner decision 2026-11-18: a present config file that does not
// parse stops boot naming the file; a BOM is accepted.

// bootBundle writes a kernel install bundle under a fresh dir and makes it the
// working directory, so config.ResolveBaseDir finds it exactly as a real boot
// does (the configs/tuning.json sentinel).
func bootBundle(t *testing.T, providers []byte) {
	t.Helper()
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "configs")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"tuning.json":    []byte(`{}`),
		"config.json":    []byte(`{"database": {"host": "localhost", "user": "u", "password": "p", "dbname": "d"}}`),
		"providers.json": providers,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(cfgDir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
}

const providersDoc = `{
  "llm_provider": {
    "default": "primary",
    "generators": [
      {"id": "primary", "provider": "openai", "model": "m", "endpoint": "http://127.0.0.1:3131/v1", "timeout_ms": 1000}
    ]
  }
}`

// A malformed providers.json stops app.Run — the production boot entry — at the
// config load, with an error naming the file and position, before anything
// listens (so there is no green /healthz in front of a kernel without an LLM).
func TestBoot_MalformedProvidersJSONRefusesBoot(t *testing.T) {
	// A trailing comma after the generator list: line 6.
	bad := strings.Replace(providersDoc, "}\n    ]", "},\n    ]", 1)
	bootBundle(t, []byte(bad))

	err := Run(context.Background(), DefaultOptions())
	if err == nil {
		t.Fatal("app.Run booted with a malformed providers.json")
	}
	if !errors.Is(err, config.ErrMalformedConfigFile) {
		t.Fatalf("boot error does not wrap config.ErrMalformedConfigFile: %v", err)
	}
	if !strings.Contains(err.Error(), "providers.json") || !strings.Contains(err.Error(), "line 6") {
		t.Fatalf("boot error does not name providers.json at line 6: %v", err)
	}
}

// A BOM-prefixed providers.json loads through the same boot load app.Run makes
// (config store opened from the base dir, then LoadConfigWithStore), and its
// generators are in effect.
func TestBoot_BOMPrefixedProvidersJSONLoads(t *testing.T) {
	bootBundle(t, append([]byte("\xEF\xBB\xBF"), providersDoc...))

	baseDir := config.ResolveBaseDir()
	store, err := OpenConfigStore(baseDir)
	if err != nil {
		t.Fatalf("OpenConfigStore: %v", err)
	}
	if store != nil {
		defer func() { _ = store.Close() }()
	}
	cfg, prov, err := config.LoadConfigWithStore(filepath.Join(baseDir, "configs", "config.json"), configStoreOrNil(store))
	if err != nil {
		t.Fatalf("boot config load refused a BOM-prefixed providers.json: %v", err)
	}
	if cfg.LLMProvider.Default != "primary" || len(cfg.LLMProvider.Generators) != 1 || cfg.LLMProvider.Generators[0].ID != "primary" {
		t.Fatalf("providers.json was not merged: default=%q generators=%+v", cfg.LLMProvider.Default, cfg.LLMProvider.Generators)
	}
	if got := prov.Source("llm_provider.default"); got != config.SourceProviders {
		t.Fatalf("llm_provider.default provenance = %q, want %q", got, config.SourceProviders)
	}
}

// The deployment integration catalog is read on the boot path
// (buildMCPConnector): one that does not parse refuses boot; one that parses
// but is invalid keeps the documented ADR-0139 D8 fallback to the built-in
// catalog; a BOM is accepted.
func TestLoadSandboxCatalog_MalformedRefusesInvalidFallsBack(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	if _, err := loadSandboxCatalog(write("bad.json", `{"schema_version": 1,`)); !errors.Is(err, config.ErrMalformedConfigFile) ||
		!strings.Contains(err.Error(), "bad.json") {
		t.Fatalf("malformed catalog: want ErrMalformedConfigFile naming bad.json, got %v", err)
	}
	cat, err := loadSandboxCatalog(write("invalid.json", `{"schema_version": 2, "entries": []}`))
	if err != nil || cat == nil || len(cat.Entries()) == 0 {
		t.Fatalf("invalid catalog: want the built-in fallback, got cat=%v err=%v", cat, err)
	}
	if cat, err := loadSandboxCatalog(write("bom.json", "\xEF\xBB\xBF"+`{"schema_version": 1, "entries": []}`)); err != nil || cat == nil {
		t.Fatalf("BOM-prefixed catalog: got cat=%v err=%v", cat, err)
	}
}
