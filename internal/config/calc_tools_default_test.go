package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
)

// ENG-897 (owner decision 2026-11-15): the ADR-0145 D4 calculation tools are on by
// default, and an explicit false in a file layer still turns them off.
func TestCalcTools_DefaultsOnAndAnOperatorCanTurnItOff(t *testing.T) {
	if !config.DefaultConfig().Execution.Tools.CalcTools {
		t.Fatal("execution.tools.calc_tools must default TRUE (owner decision 2026-11-15)")
	}
	load := func(t *testing.T, body string) *config.Config {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		return cfg
	}
	base := `"database":{"host":"localhost","port":"5432","user":"u","password":"p","dbname":"d"},"server":{"port":"50051"}`
	if cfg := load(t, `{`+base+`}`); !cfg.Execution.Tools.CalcTools {
		t.Error("a config that does not mention calc_tools must load it as true")
	}
	if cfg := load(t, `{"schema_version":2,`+base+`,"execution":{"tools":{"calc_tools":false}}}`); cfg.Execution.Tools.CalcTools {
		t.Error("an explicit execution.tools.calc_tools=false must turn the tools off")
	}
}
