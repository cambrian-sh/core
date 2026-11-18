package integrations

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/internal/config"
)

// ENG-2121: the deployment catalog (mcp.catalog_file) is a config file the
// kernel reads at boot. A leading UTF-8 BOM is accepted; a file that does not
// decode is reported as config.ErrMalformedConfigFile naming the file and line,
// which boot turns into a refusal (app.loadSandboxCatalog).
func TestDeploymentCatalog_BOMAcceptedMalformedNamed(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	if err := os.WriteFile(good, []byte("\xEF\xBB\xBF"+`{"schema_version": 1, "entries": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(good); err != nil {
		t.Fatalf("a BOM-prefixed deployment catalog was refused: %v", err)
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{\n  \"schema_version\": 1,\n}"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(bad)
	if !errors.Is(err, config.ErrMalformedConfigFile) {
		t.Fatalf("a malformed catalog is not ErrMalformedConfigFile: %v", err)
	}
	if !strings.Contains(err.Error(), bad) || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("error does not name %s at line 3: %v", bad, err)
	}

	// A catalog that parses but is invalid stays an ordinary error: boot keeps
	// the documented ADR-0139 D8 fallback to the built-in catalog for it.
	invalid := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(invalid, []byte(`{"schema_version": 2, "entries": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(invalid); err == nil || errors.Is(err, config.ErrMalformedConfigFile) {
		t.Fatalf("an invalid (but parseable) catalog: want a plain error, got %v", err)
	}
}
