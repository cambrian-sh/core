package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ENG-2121: a config file layer that is present but does not parse used to be
// skipped in silence (loadIfPresent discarded koanf's error), so a providers.json
// saved with a UTF-8 BOM booted a kernel with no LLM. These tests pin the owner's
// decision for EVERY file layer FileLayers names: a leading BOM is accepted, and
// a malformed file stops the load with an error naming the file and position.

const bomTestAlpha = 0.37

// layerBody is a layer document setting execution.supervision.ewma_alpha; the
// config.json layer also carries the database block baseBundle would have.
func layerBody(source string) string {
	db := ""
	if source == SourceConfig {
		db = `"database": {"host": "localhost", "user": "u", "password": "p", "dbname": "d"},` + "\n"
	}
	return "{\n" + db + `"execution": {"supervision": {"ewma_alpha": 0.37}}` + "\n}\n"
}

// bundleFor returns a bundle dir with the base config.json, unless the layer
// under test IS config.json (it is then written by the test itself).
func bundleFor(t *testing.T, source string) string {
	t.Helper()
	if source == SourceConfig {
		return t.TempDir()
	}
	return baseBundle(t)
}

func TestFileLayers_AcceptLeadingBOM(t *testing.T) {
	for _, layer := range FileLayers(filepath.Join("x", "config.json")) {
		t.Run(layer.Source, func(t *testing.T) {
			dir := bundleFor(t, layer.Source)
			name := filepath.Base(layer.Path)
			writeCfg(t, dir, name, string(utf8BOM)+layerBody(layer.Source))

			cfg, prov := loadFrom(t, dir, nil)
			if got := cfg.Execution.Supervision.EWMAAlpha; got != bomTestAlpha {
				t.Fatalf("ewma_alpha = %v, want %v: the BOM-prefixed %s was not merged", got, bomTestAlpha, name)
			}
			if got := prov.Source("execution.supervision.ewma_alpha"); got != layer.Source {
				t.Fatalf("ewma_alpha provenance = %q, want %q", got, layer.Source)
			}
		})
	}
}

func TestFileLayers_MalformedFileRefusesLoadNamingFile(t *testing.T) {
	// The trailing comma sits on line 3; the offending '}' is column 40.
	const malformed = "{\n  \"execution\": {\n    \"supervision\": {\"ewma_alpha\": 0.37,}\n  }\n}\n"
	for _, layer := range FileLayers(filepath.Join("x", "config.json")) {
		t.Run(layer.Source, func(t *testing.T) {
			dir := bundleFor(t, layer.Source)
			name := filepath.Base(layer.Path)
			writeCfg(t, dir, name, malformed)

			_, _, err := LoadConfigWithStore(filepath.Join(dir, "config.json"), nil)
			if err == nil {
				t.Fatalf("a malformed %s loaded without error — the layer was skipped in silence", name)
			}
			if !errors.Is(err, ErrMalformedConfigFile) {
				t.Fatalf("error does not wrap ErrMalformedConfigFile: %v", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, filepath.Join(dir, name)) {
				t.Fatalf("error does not name the file %s: %v", filepath.Join(dir, name), err)
			}
			if !strings.Contains(msg, "line 3, column 40") {
				t.Fatalf("error does not give the position (line 3, column 40): %v", err)
			}
		})
	}
}

func TestFileLayers_BOMAndMalformedStillRefused(t *testing.T) {
	// Stripping the BOM must not hide what follows it.
	dir := baseBundle(t)
	writeCfg(t, dir, "providers.json", string(utf8BOM)+`{"llm_provider": {`)
	_, _, err := LoadConfigWithStore(filepath.Join(dir, "config.json"), nil)
	if !errors.Is(err, ErrMalformedConfigFile) || !strings.Contains(err.Error(), "providers.json") {
		t.Fatalf("want a malformed-file error naming providers.json, got %v", err)
	}
}

func TestFileLayers_NonObjectDocumentRefused(t *testing.T) {
	// Valid JSON that is not an object cannot be merged as a layer either.
	dir := baseBundle(t)
	writeCfg(t, dir, "tuning.local.json", `["execution"]`)
	_, _, err := LoadConfigWithStore(filepath.Join(dir, "config.json"), nil)
	if !errors.Is(err, ErrMalformedConfigFile) || !strings.Contains(err.Error(), "tuning.local.json") {
		t.Fatalf("want a malformed-file error naming tuning.local.json, got %v", err)
	}
}

func TestFileLayers_EmptyOrMissingFileIsSkipped(t *testing.T) {
	// A missing optional layer stays fine; so does a file with nothing in it
	// (a touched placeholder, or a BOM and a newline from an editor).
	dir := baseBundle(t)
	writeCfg(t, dir, "tuning.local.json", "")
	writeCfg(t, dir, "embedder.local.json", string(utf8BOM)+"\r\n")
	cfg, _ := loadFrom(t, dir, nil)
	if cfg.Database.Host != "localhost" {
		t.Fatalf("database.host = %q; the base config.json did not load", cfg.Database.Host)
	}
}

func TestResolveFileSetting_SkipsMalformedAndReadsBOMFile(t *testing.T) {
	dir := baseBundle(t)
	writeCfg(t, dir, "providers.json", string(utf8BOM)+`{"llm_provider": {"default": "a"}}`)
	writeCfg(t, dir, "providers.local.json", `{"llm_provider": {"default": "b",}}`)
	o, ok := ResolveFileSetting(filepath.Join(dir, "config.json"), "llm_provider.default")
	if !ok || o.Source != SourceProviders || o.Value != "a" {
		t.Fatalf("ResolveFileSetting = %+v, %v; want providers.json (BOM file read, malformed local skipped)", o, ok)
	}
}

func TestMigrateFileInPlace_BOMKeptAndMalformedNamed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tuning.local.json")
	if err := os.WriteFile(path, append(append([]byte{}, utf8BOM...), `{"execution": {"ewma_alpha": 0.5}}`...), 0o600); err != nil {
		t.Fatal(err)
	}
	moved, err := MigrateFileInPlace(path)
	if err != nil {
		t.Fatalf("MigrateFileInPlace on a BOM-prefixed v1 file: %v", err)
	}
	if len(moved) == 0 {
		t.Fatal("the v1 flat key was not migrated")
	}
	out, _ := os.ReadFile(path)
	if !bytes.HasPrefix(out, utf8BOM) {
		t.Fatal("the rewrite dropped the operator's BOM")
	}
	if b, err := migrateFile(path); err != nil || b == nil {
		t.Fatalf("the migrated file no longer loads: %v", err)
	}

	writeCfg(t, dir, "bad.json", "{\n\"execution\": }")
	_, err = MigrateFileInPlace(filepath.Join(dir, "bad.json"))
	if !errors.Is(err, ErrMalformedConfigFile) || !strings.Contains(err.Error(), "bad.json") || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("want a malformed-file error naming bad.json at line 2, got %v", err)
	}
}

func TestLineCol(t *testing.T) {
	b := []byte("ab\ncd\nef")
	for _, tc := range []struct {
		off       int64
		line, col int
	}{
		{1, 1, 1}, {2, 1, 2}, {4, 2, 1}, {5, 2, 2}, {8, 3, 2}, {99, 3, 2},
	} {
		if l, c := lineCol(b, tc.off); l != tc.line || c != tc.col {
			t.Errorf("lineCol(%d) = %d:%d, want %d:%d", tc.off, l, c, tc.line, tc.col)
		}
	}
}
