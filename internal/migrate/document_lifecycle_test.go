package migrate

import (
	"strings"
	"testing"
)

// ADR-0138 D3: 0026 adds the document's writer of record, content digest and version, and
// the content-free version history. Additive only: older binaries keep booting.
func TestLoadMigrations_Includes0026DocumentLifecycle(t *testing.T) {
	migs, err := loadMigrations(1536)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version != 26 {
			continue
		}
		if m.name != "document_lifecycle" {
			t.Fatalf("migration 26 is %q", m.name)
		}
		for _, s := range []string{
			"ALTER TABLE documents ADD COLUMN IF NOT EXISTS written_by TEXT",
			"ALTER TABLE documents ADD COLUMN IF NOT EXISTS content_digest TEXT",
			"ALTER TABLE documents ADD COLUMN IF NOT EXISTS version INT NOT NULL DEFAULT 1",
			"CREATE TABLE IF NOT EXISTS document_versions",
			"PRIMARY KEY (document_id, version)",
		} {
			if !strings.Contains(m.sql, s) {
				t.Errorf("0026 lacks %q", s)
			}
		}
		for _, line := range strings.Split(m.sql, "\n") {
			l := strings.TrimSpace(line)
			if strings.HasPrefix(l, "--") {
				continue
			}
			if u := strings.ToUpper(l); strings.Contains(u, "DROP ") || strings.Contains(u, " TEXT ") && strings.Contains(u, "BODY") {
				t.Errorf("0026 must be additive and content-free: %q", l)
			}
		}
		return
	}
	t.Fatal("0026_document_lifecycle is not embedded")
}
