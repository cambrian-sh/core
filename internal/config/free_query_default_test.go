package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
)

// ENG-2232 (owner ruling 2026-11-19): free-query tools need an explicit grant
// by default; a file that does not mention the block keeps the default, and an
// operator's explicit false survives the merge over it.
func TestFreeQuery_DefaultRequiresExplicitGrantAndFalseSurvives(t *testing.T) {
	if !config.DefaultConfig().Execution.Tools.FreeQuery.RequireExplicitGrant {
		t.Fatal("default must require an explicit grant")
	}
	load := func(t *testing.T, body string) *config.Config {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := config.LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	base := `"database":{"host":"localhost","port":"5432","user":"u","password":"p","dbname":"d"},"server":{"port":"50051"}`
	if !load(t, `{`+base+`}`).Execution.Tools.FreeQuery.RequireExplicitGrant {
		t.Error("a config without execution.tools.free_query must keep the guard on")
	}
	if load(t, `{"schema_version":4,`+base+`,"execution":{"tools":{"free_query":{"require_explicit_grant":false}}}}`).Execution.Tools.FreeQuery.RequireExplicitGrant {
		t.Error("an explicit false was lost")
	}
}
