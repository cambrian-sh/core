package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// ENG-2061: a kernel booted with NO config.json (a container wired only through
// CAMBRIAN_* env vars) must get the infrastructure values config.example.json ships,
// not empty strings. An empty storage.db_name made bbolt open the data directory.
func TestLoadConfig_NoConfigFile_InfrastructureDefaults(t *testing.T) {
	t.Setenv("CAMBRIAN_DATABASE__PASSWORD", "p") // the one required key with no default: a secret

	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("a boot with no config.json and only the password in env must load: %v", err)
	}
	for _, c := range []struct{ key, got, want string }{
		{"metabolism.agents_dir", cfg.Metabolism.AgentsDir, "agents"},
		{"storage.data_dir", cfg.Storage.DataDir, "data"},
		{"storage.db_name", cfg.Storage.DBName, "agents.db"},
		{"database.host", cfg.Database.Host, "localhost"},
		{"database.port", cfg.Database.Port, "5432"},
		{"database.user", cfg.Database.User, "cambrian"},
		{"database.dbname", cfg.Database.DBName, "cambrian_db"},
		{"server.port", cfg.Server.Port, "50051"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q with no config.json; want %q", c.key, c.got, c.want)
		}
	}
}

// An env override still wins over the new defaults (the container path).
func TestLoadConfig_NoConfigFile_EnvOverridesDefaults(t *testing.T) {
	t.Setenv("CAMBRIAN_DATABASE__PASSWORD", "p")
	t.Setenv("CAMBRIAN_DATABASE__HOST", "pg")
	t.Setenv("CAMBRIAN_STORAGE__DB_NAME", "other.db")
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.Host != "pg" || cfg.Storage.DBName != "other.db" {
		t.Fatalf("env must override the defaults: host=%q db_name=%q", cfg.Database.Host, cfg.Storage.DBName)
	}
}

// The Go defaults and the shipped example agree, so copying the example changes
// nothing a fresh boot would not already do (paths compared after cleaning:
// the example writes "./agents").
func TestDefaultConfig_MatchesConfigExampleInfrastructure(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "configs", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ex struct {
		Metabolism struct {
			AgentsDir string `json:"agents_dir"`
		} `json:"metabolism"`
		Storage struct {
			DataDir string `json:"data_dir"`
			DBName  string `json:"db_name"`
		} `json:"storage"`
		Database struct {
			Host   string `json:"host"`
			Port   string `json:"port"`
			User   string `json:"user"`
			DBName string `json:"dbname"`
		} `json:"database"`
		Server struct {
			Port string `json:"port"`
		} `json:"server"`
	}
	if err := json.Unmarshal(b, &ex); err != nil {
		t.Fatal(err)
	}
	d := DefaultConfig()
	for _, c := range []struct{ key, def, example string }{
		{"metabolism.agents_dir", d.Metabolism.AgentsDir, filepath.Clean(ex.Metabolism.AgentsDir)},
		{"storage.data_dir", d.Storage.DataDir, ex.Storage.DataDir},
		{"storage.db_name", d.Storage.DBName, ex.Storage.DBName},
		{"database.host", d.Database.Host, ex.Database.Host},
		{"database.port", d.Database.Port, ex.Database.Port},
		{"database.user", d.Database.User, ex.Database.User},
		{"database.dbname", d.Database.DBName, ex.Database.DBName},
		{"server.port", d.Server.Port, ex.Server.Port},
	} {
		if c.def != c.example {
			t.Errorf("%s: DefaultConfig %q, config.example.json %q — keep them equal", c.key, c.def, c.example)
		}
	}
}
