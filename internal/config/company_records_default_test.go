package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
)

// ENG-2117 (owner decision 2026-11-18): company_records is on by default, an
// explicit false in a file layer turns it off, and its bounds are validated.
func TestCompanyRecords_DefaultsOnAndAnOperatorCanTurnItOff(t *testing.T) {
	if !config.DefaultConfig().Execution.Tools.CompanyRecords.Enabled {
		t.Fatal("execution.tools.company_records.enabled must default TRUE (owner decision 2026-11-18)")
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
	if err != nil || !cfg.Execution.Tools.CompanyRecords.Enabled {
		t.Fatalf("a config that does not mention company_records must load it on: %v", err)
	}
	cfg, err = load(t, `{"schema_version":4,`+base+`,"execution":{"tools":{"company_records":{"enabled":false,"max_rows":20}}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if cr := cfg.Execution.Tools.CompanyRecords; cr.Enabled || cr.MaxRows != 20 {
		t.Errorf("explicit enabled=false / max_rows=20 not honoured: %+v", cr)
	}
	bad := config.DefaultConfig()
	bad.Execution.Tools.CompanyRecords.MaxEvents = 5000
	if e := bad.Validate(); e == nil || !strings.Contains(e.Error(), "company_records") {
		t.Errorf("max_events above 1000 must be refused, got %v", e)
	}
}
