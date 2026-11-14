package migrate

import (
	"strings"
	"testing"
)

// 0049_drop_ebbinghaus_decay.sql (ENG-74, owner decision 2026-11-14): the decay
// function is dropped, idempotently, and nothing else is.
func TestLoadMigrations_Includes0049DropEbbinghausDecay(t *testing.T) {
	migs, err := loadMigrations(4)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version != 49 {
			continue
		}
		if m.name != "drop_ebbinghaus_decay" {
			t.Fatalf("migration 49 is %q", m.name)
		}
		if !strings.Contains(m.sql, "DROP FUNCTION IF EXISTS apply_ebbinghaus_decay(INT);") {
			t.Error("0049 must drop apply_ebbinghaus_decay(INT) idempotently")
		}
		up := strings.ToUpper(m.sql)
		if strings.Contains(up, "DELETE FROM") || strings.Count(up, "DROP FUNCTION") != 1 {
			t.Error("0049 must drop only the decay function")
		}
		return
	}
	t.Fatal("migration 0049 not loaded")
}
