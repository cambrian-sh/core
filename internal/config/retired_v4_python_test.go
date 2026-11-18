package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Schema v4 (ADR-0157 D7.14, owner A1-Q13, ENG-2008) retires
// metabolism.python_executable: Python agents and Python system tools run only
// on the kernel-managed env. A deployment that still sets it keeps starting —
// in a file, the ADR-0101 store or CAMBRIAN_METABOLISM__PYTHON_EXECUTABLE — the
// value is dropped with a WARN naming D7, and the layer's other keys stay.
func TestRetiredV4PythonExecutable_EveryLayerDropsItWithAWarn(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	retiredLogged.Range(func(k, _ any) bool { retiredLogged.Delete(k); return true })

	dir := t.TempDir()
	writeCfg(t, dir, "config.json", `{"schema_version": 3,
	  "database": {"host": "localhost", "user": "u", "password": "p", "dbname": "d"},
	  "metabolism": {"python_executable": "C:/Python313/python.exe", "agents_dir": "/kernel/agents", "bun_executable": "/opt/bun"}
	}`)
	t.Setenv("CAMBRIAN_METABOLISM__PYTHON_EXECUTABLE", "/usr/bin/python3")
	store := mapStore{"metabolism.python_executable": "/opt/venv/bin/python"}

	cfg, prov, unused, err := loadMergedReport(filepath.Join(dir, "config.json"), store)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a config carrying the retired key must still validate: %v", err)
	}
	if cfg.Metabolism.AgentsDir != "/kernel/agents" || cfg.Metabolism.BunExecutable != "/opt/bun" {
		t.Errorf("the same layer's other metabolism keys were lost: %+v", cfg.Metabolism)
	}
	if src, ok := prov["metabolism.python_executable"]; ok && src != SourceDefault {
		t.Errorf("retired key still claimed by %s", src)
	}
	for _, k := range unused {
		if strings.HasSuffix(k, "python_executable") {
			t.Errorf("retired key reached the decoder (unused %q)", k)
		}
	}
	logs := buf.String()
	if !strings.Contains(logs, "schema v4") || !strings.Contains(logs, "ADR-0157 D7") || !strings.Contains(logs, "metabolism.python_executable") {
		t.Errorf("no v4 retirement WARN naming D7:\n%s", logs)
	}
	if strings.Contains(logs, "it never had an effect") && strings.Contains(logs, "key=metabolism.python_executable") &&
		strings.Contains(logs, "schema v3") {
		t.Errorf("python_executable had an effect; it must not be logged as a v3 no-op:\n%s", logs)
	}
	if CurrentSchemaVersion != SchemaV4PythonRetired {
		t.Errorf("CurrentSchemaVersion = %d, want %d", CurrentSchemaVersion, SchemaV4PythonRetired)
	}
}

// The explicit `config-migrate` removes it and stamps v4; nothing else moves.
func TestMigrateFileInPlace_RemovesPythonExecutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeCfg(t, filepath.Dir(path), "config.json", `{"schema_version": 3,
	  "metabolism": {"python_executable": "./.venv/bin/python", "agents_dir": "./agents"}}`)
	got, err := MigrateFileInPlace(path)
	if err != nil || len(got) != 1 || got[0] != "metabolism.python_executable" {
		t.Fatalf("migrate = %v, %v", got, err)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "python_executable") || !strings.Contains(string(b), `"agents_dir": "./agents"`) ||
		!strings.Contains(string(b), `"schema_version": 4`) {
		t.Errorf("migrated file:\n%s", b)
	}
}
