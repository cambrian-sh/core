package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
)

// ENG-2169: the drainer's per-pipeline slice and worker count default to 64 /
// 4, a file layer overrides them, and out-of-range values are refused.
func TestPipelinesDrainer_SliceAndWorkersDefaultsOverrideAndBounds(t *testing.T) {
	def := config.DefaultConfig().Execution.Pipelines
	if def.DrainerSliceTasks != 64 || def.DrainerWorkers != 4 {
		t.Fatalf("defaults = slice %d / workers %d, want 64 / 4", def.DrainerSliceTasks, def.DrainerWorkers)
	}
	load := func(t *testing.T, body string) (*config.Config, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return config.LoadConfig(path)
	}
	base := `"database":{"host":"localhost","port":"5432","user":"u","password":"p","dbname":"d"},"server":{"port":"50051"}`
	cfg, err := load(t, `{`+base+`}`)
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.Execution.Pipelines; p.DrainerSliceTasks != 64 || p.DrainerWorkers != 4 {
		t.Errorf("a config that does not mention them must load the defaults: %+v", p)
	}
	cfg, err = load(t, `{"schema_version":4,`+base+`,"execution":{"pipelines":{"drainer_slice_tasks":8,"drainer_workers":1}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.Execution.Pipelines; p.DrainerSliceTasks != 8 || p.DrainerWorkers != 1 || !p.DrainerEnabled {
		t.Errorf("explicit slice 8 / workers 1 not honoured (or drainer_enabled lost): %+v", p)
	}
	for _, mut := range []func(*config.Config){
		func(c *config.Config) { c.Execution.Pipelines.DrainerWorkers = 65 },
		func(c *config.Config) { c.Execution.Pipelines.DrainerWorkers = -1 },
		func(c *config.Config) { c.Execution.Pipelines.DrainerSliceTasks = -1 },
	} {
		bad := config.DefaultConfig()
		mut(bad)
		if e := bad.Validate(); e == nil || !strings.Contains(e.Error(), "drainer_") {
			t.Errorf("out-of-range drainer setting must be refused, got %v", e)
		}
	}
}
