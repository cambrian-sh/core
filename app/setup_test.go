package app

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUVAssetMapping(t *testing.T) {
	cases := []struct {
		goos, goarch string
		want         string
		ok           bool
	}{
		{"linux", "amd64", "uv-x86_64-unknown-linux-gnu.tar.gz", true},
		{"linux", "arm64", "uv-aarch64-unknown-linux-gnu.tar.gz", true},
		{"windows", "amd64", "uv-x86_64-pc-windows-msvc.zip", true},
		{"windows", "arm64", "uv-aarch64-pc-windows-msvc.zip", true},
		{"darwin", "arm64", "uv-aarch64-apple-darwin.tar.gz", true},
		{"linux", "riscv64", "", false},
		{"plan9", "amd64", "", false},
	}
	for _, c := range cases {
		got, ok := uvAsset(c.goos, c.goarch)
		if got != c.want || ok != c.ok {
			t.Errorf("uvAsset(%s,%s) = %q,%v; want %q,%v", c.goos, c.goarch, got, ok, c.want, c.ok)
		}
	}
}

// The embed must keep Python package markers (default embed rules would drop
// underscore-prefixed __init__.py — that's why the directive uses `all:`) while
// unpack filters build junk.
func TestUnpackAgentsKeepsInitFiltersJunk(t *testing.T) {
	dst := t.TempDir()
	n, err := unpackAgents(dst)
	if err != nil {
		t.Fatalf("unpackAgents: %v", err)
	}
	if n == 0 {
		t.Fatal("unpackAgents wrote no files")
	}
	for _, must := range []string{
		"requirements.lock",
		filepath.Join("system", "docling_agent", "__init__.py"),
		filepath.Join("system", "docling_agent", "agent.py"),
	} {
		if !fileExists(filepath.Join(dst, must)) {
			t.Errorf("expected %s to be unpacked", must)
		}
	}
	err = filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "__pycache__" {
			t.Errorf("build junk unpacked: %s", p)
		}
		if !d.IsDir() && strings.HasSuffix(p, ".pyc") {
			t.Errorf("build junk unpacked: %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

func TestWriteConfigBundleFreshAndIdempotent(t *testing.T) {
	prefix := t.TempDir()
	s := &setupState{
		ui:         newSetupUI(true),
		prefix:     prefix,
		serverPort: "50051",
		db:         setupDB{host: "db.example", port: "5433", user: "u", password: "p", dbname: "d"},
	}

	if err := s.writeConfigBundle(); err != nil {
		t.Fatalf("writeConfigBundle (fresh): %v", err)
	}
	cfgPath := filepath.Join(prefix, "configs", "config.json")
	var cfg map[string]any
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if _, has := cfg["_comment"]; has {
		t.Error("_comment should be stripped")
	}
	db := cfg["database"].(map[string]any)
	if db["host"] != "db.example" || db["port"] != "5433" {
		t.Errorf("database block not applied: %v", db)
	}
	emb := cfg["embedder"].(map[string]any)
	if emb["dimensions"].(float64) != 1024 {
		t.Errorf("embedder dimensions = %v, want 1024", emb["dimensions"])
	}
	st := cfg["storage"].(map[string]any)
	if st["data_dir"] != filepath.Join(prefix, "data") {
		t.Errorf("storage.data_dir = %v", st["data_dir"])
	}
	met := cfg["metabolism"].(map[string]any)
	if met["agents_dir"] != filepath.Join(prefix, "agents") {
		t.Errorf("metabolism.agents_dir = %v", met["agents_dir"])
	}
	if !fileExists(filepath.Join(prefix, "configs", "tuning.json")) {
		t.Error("tuning.json sentinel not written")
	}
	if fi, err := os.Stat(filepath.Join(prefix, "data")); err != nil || !fi.IsDir() {
		t.Error("data dir not created")
	}

	// Re-run must preserve hand-edits (custom keys, an existing embedder block).
	cfg["custom_key"] = "keep-me"
	cfg["embedder"] = map[string]any{"provider": "custom", "dimensions": float64(768)}
	edited, _ := json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, edited, 0o644); err != nil {
		t.Fatalf("write edited config: %v", err)
	}
	s.db.host = "db2.example"
	if err := s.writeConfigBundle(); err != nil {
		t.Fatalf("writeConfigBundle (rerun): %v", err)
	}
	b, _ = os.ReadFile(cfgPath)
	var cfg2 map[string]any
	if err := json.Unmarshal(b, &cfg2); err != nil {
		t.Fatalf("parse rerun config: %v", err)
	}
	if cfg2["custom_key"] != "keep-me" {
		t.Error("rerun dropped a hand-edited key")
	}
	if cfg2["embedder"].(map[string]any)["provider"] != "custom" {
		t.Error("rerun overwrote an existing embedder block")
	}
	if cfg2["database"].(map[string]any)["host"] != "db2.example" {
		t.Error("rerun did not apply the new database host")
	}
}

// A BOM'd config must merge (Windows editors add BOMs); an unparseable config
// must be refused, never clobbered — both defects were caught by the live E2E.
func TestWriteConfigBundleBOMAndInvalid(t *testing.T) {
	prefix := t.TempDir()
	s := &setupState{ui: newSetupUI(true), prefix: prefix, serverPort: "50051",
		db: setupDB{host: "h", port: "5432", user: "u", password: "p", dbname: "d"}}
	cfgDir := filepath.Join(prefix, "configs")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(cfgDir, "config.json")

	bom := append([]byte{0xEF, 0xBB, 0xBF}, []byte(`{"server":{"port":"50099"},"keep":"me"}`)...)
	if err := os.WriteFile(cfgPath, bom, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.writeConfigBundle(); err != nil {
		t.Fatalf("BOM'd config should merge, got: %v", err)
	}
	if s.serverPort != "50099" {
		t.Errorf("server port from BOM'd config = %s, want 50099", s.serverPort)
	}
	b, _ := os.ReadFile(cfgPath)
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["keep"] != "me" {
		t.Error("BOM'd config's keys were not preserved")
	}

	if err := os.WriteFile(cfgPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.writeConfigBundle(); err == nil {
		t.Fatal("unparseable config must be refused, not overwritten")
	}
	b, _ = os.ReadFile(cfgPath)
	if string(b) != "{not json" {
		t.Error("unparseable config was modified")
	}
}

func TestParseSelfCheck(t *testing.T) {
	out := "some noise\nMISSING\tdocling_agent\tdocling,torch\nMISSING\tagents\trequests\ntrailing"
	got := parseSelfCheck(out)
	if len(got) != 2 {
		t.Fatalf("parseSelfCheck returned %d entries, want 2", len(got))
	}
	if got[0].agent != "docling_agent" || got[0].mods != "docling,torch" {
		t.Errorf("first entry = %+v", got[0])
	}
	if parseSelfCheck("all good") != nil {
		t.Error("clean output should parse to nil")
	}
}

func TestEnvFileUpsertPreservesAndReplaces(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	seed := "# premium knobs\nLANGFUSE_HOST=http://x\nCAMBRIAN_OPERATOR_USER=old\n"
	if err := os.WriteFile(p, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	err := upsertEnvFile(p, map[string]string{
		"CAMBRIAN_OPERATOR_USER":     "operator",
		"CAMBRIAN_OPERATOR_PASSWORD": "s3cret",
	})
	if err != nil {
		t.Fatalf("upsertEnvFile: %v", err)
	}
	got, err := readEnvFile(p)
	if err != nil {
		t.Fatalf("readEnvFile: %v", err)
	}
	if got["LANGFUSE_HOST"] != "http://x" {
		t.Error("unrelated key was not preserved")
	}
	if got["CAMBRIAN_OPERATOR_USER"] != "operator" || got["CAMBRIAN_OPERATOR_PASSWORD"] != "s3cret" {
		t.Errorf("operator keys wrong: %v", got)
	}
	raw, _ := os.ReadFile(p)
	if !strings.Contains(string(raw), "# premium knobs") {
		t.Error("comment line was dropped")
	}
	if strings.Contains(string(raw), "old") {
		t.Error("replaced value still present")
	}
	// Missing file reads as empty, not an error.
	if m, err := readEnvFile(filepath.Join(t.TempDir(), "absent")); err != nil || len(m) != 0 {
		t.Errorf("missing file: %v %v", m, err)
	}
}

func TestGeneratePassword(t *testing.T) {
	a, err := generatePassword()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := generatePassword()
	if len(a) < 20 || a == b {
		t.Errorf("weak generation: %q %q", a, b)
	}
}

// A re-run must keep existing operator credentials untouched (rotation happens
// by editing .env, not by re-running setup).
func TestStepOperatorIdempotent(t *testing.T) {
	t.Setenv("CAMBRIAN_OPERATOR_USER", "")
	t.Setenv("CAMBRIAN_OPERATOR_PASSWORD", "")
	prefix := t.TempDir()
	s := &setupState{ui: newSetupUI(true), prefix: prefix}
	envPath := filepath.Join(prefix, ".env")
	if err := os.WriteFile(envPath, []byte("CAMBRIAN_OPERATOR_USER=alice\nCAMBRIAN_OPERATOR_PASSWORD=keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.stepOperator()
	got, _ := readEnvFile(envPath)
	if got["CAMBRIAN_OPERATOR_USER"] != "alice" || got["CAMBRIAN_OPERATOR_PASSWORD"] != "keep" {
		t.Errorf("re-run modified existing credentials: %v", got)
	}
	if s.degraded {
		t.Error("idempotent path must not degrade")
	}

	// Fresh prefix + non-interactive ⇒ a random password is generated and written.
	// (Re-clear the env: the idempotent path above exported alice/keep.)
	t.Setenv("CAMBRIAN_OPERATOR_USER", "")
	t.Setenv("CAMBRIAN_OPERATOR_PASSWORD", "")
	prefix2 := t.TempDir()
	s2 := &setupState{ui: newSetupUI(true), prefix: prefix2}
	s2.stepOperator()
	got2, _ := readEnvFile(filepath.Join(prefix2, ".env"))
	if got2["CAMBRIAN_OPERATOR_USER"] != "operator" {
		t.Errorf("default username wrong: %v", got2)
	}
	if len(got2["CAMBRIAN_OPERATOR_PASSWORD"]) < 20 {
		t.Errorf("generated password missing/weak: %q", got2["CAMBRIAN_OPERATOR_PASSWORD"])
	}
	if s2.degraded {
		t.Error("generation path must not degrade")
	}
}

func TestVenvPythonPath(t *testing.T) {
	p := venvPython(filepath.Join("x", "venv"))
	if runtime.GOOS == "windows" {
		if !strings.HasSuffix(p, filepath.Join("Scripts", "python.exe")) {
			t.Errorf("venvPython = %s", p)
		}
	} else if !strings.HasSuffix(p, filepath.Join("bin", "python")) {
		t.Errorf("venvPython = %s", p)
	}
}
