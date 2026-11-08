package migrate

import (
	"strings"
	"testing"
)

// 0032 adds the ADR-0142 principal registry. Additive only: an existing
// deployment's tables are untouched, and an older binary ignores the new ones.
func TestLoadMigrations_Includes0032PrincipalRegistry(t *testing.T) {
	migs, err := loadMigrations(1536)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version != 32 {
			continue
		}
		if m.name != "principal_registry" {
			t.Fatalf("migration 32 is %q", m.name)
		}
		for _, s := range []string{
			"CREATE TABLE IF NOT EXISTS principals",
			"CHECK (kind IN ('user', 'actor'))",
			"CREATE TABLE IF NOT EXISTS principal_identities",
			"PRIMARY KEY (issuer, subject)",
			"CREATE TABLE IF NOT EXISTS principal_aliases",
			"CREATE TABLE IF NOT EXISTS principal_local_credentials",
		} {
			if !strings.Contains(m.sql, s) {
				t.Errorf("0032 lacks %q", s)
			}
		}
		for _, line := range strings.Split(m.sql, "\n") {
			l := strings.ToUpper(strings.TrimSpace(line))
			if !strings.HasPrefix(l, "--") && (strings.Contains(l, "DROP ") || strings.Contains(l, "ALTER TABLE")) {
				t.Errorf("0032 must be additive: %q", line)
			}
		}
		return
	}
	t.Fatal("0032_principal_registry is not embedded")
}
