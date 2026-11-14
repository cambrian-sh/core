package migrate

import (
	"strings"
	"testing"
)

// 0048_entities_namespace_key.sql (ENG-1043 item 1): entities are keyed by
// (namespace_id, id). The behaviour is proved against Postgres by
// postgres.TestPgEntityStore_SameIDMintsPerNamespace; this pins the file.
func TestLoadMigrations_Includes0048EntitiesNamespaceKey(t *testing.T) {
	migs, err := loadMigrations(4)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version != 48 {
			continue
		}
		if m.name != "entities_namespace_key" {
			t.Fatalf("migration 48 is %q", m.name)
		}
		for _, s := range []string{
			"DROP CONSTRAINT IF EXISTS entities_pkey",
			"PRIMARY KEY (namespace_id, id)",
			"idx_entities_id ON entities (id)",
		} {
			if !strings.Contains(m.sql, s) {
				t.Errorf("0048 lacks %q", s)
			}
		}
		if strings.Contains(strings.ToUpper(m.sql), "DELETE FROM") {
			t.Error("0048 must not delete rows: existing entities keep working")
		}
		return
	}
	t.Fatal("migration 0048 not loaded")
}
