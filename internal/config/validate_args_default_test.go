package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
)

// TestValidateArgs_DefaultsOnAndAnOperatorCanTurnItOff pins the owner decision of
// 2026-11-14 (third round, T-20261104-toolexecutor-never-validates-tool-arguments):
// ADR-0145 D9 argument validation is on by default, and an explicit false in a
// file layer still turns it off (the zero-value-over-true-default path).
func TestValidateArgs_DefaultsOnAndAnOperatorCanTurnItOff(t *testing.T) {
	if !config.DefaultConfig().Execution.Tools.ValidateArgs {
		t.Fatal("execution.tools.validate_args must default TRUE (owner decision 2026-11-14)")
	}

	load := func(t *testing.T, body string) *config.Config {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}
		cfg, err := config.LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		return cfg
	}

	if cfg := load(t, `{"database":{"host":"localhost","port":"5432","user":"u","password":"p","dbname":"d"},"server":{"port":"50051"}}`); !cfg.Execution.Tools.ValidateArgs {
		t.Error("a config that does not mention validate_args must load it as true")
	}
	off := `{"schema_version":2,"database":{"host":"localhost","port":"5432","user":"u","password":"p","dbname":"d"},"server":{"port":"50051"},"execution":{"tools":{"validate_args":false}}}`
	if cfg := load(t, off); cfg.Execution.Tools.ValidateArgs {
		t.Error("an explicit execution.tools.validate_args=false must turn validation off")
	}
}
