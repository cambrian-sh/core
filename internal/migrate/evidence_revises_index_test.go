package migrate

import (
	"strings"
	"testing"
)

// 0050_evidence_revises_index.sql (ENG-1026): the latest-revision probe is indexed.
func TestLoadMigrations_Includes0050EvidenceRevisesIndex(t *testing.T) {
	migs, err := loadMigrations(4)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version == 50 {
			if m.name != "evidence_revises_index" || !strings.Contains(m.sql, "CREATE INDEX IF NOT EXISTS idx_evidence_revises_id ON evidence (revises_id)") {
				t.Fatalf("0050 = %q: %s", m.name, m.sql)
			}
			return
		}
	}
	t.Fatal("migration 0050 not loaded")
}
