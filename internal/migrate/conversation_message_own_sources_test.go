package migrate

import (
	"strings"
	"testing"
)

// 0063_conversation_message_own_sources.sql (ENG-2165): one nullable column, the reply's
// own refs. Not destructive.
func TestLoadMigrations_Includes0063OwnSources(t *testing.T) {
	migs, err := loadMigrations(4)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version != 63 {
			continue
		}
		if m.name != "conversation_message_own_sources" {
			t.Fatalf("migration 63 is %q", m.name)
		}
		if IsDestructive(m.sql) || !strings.Contains(m.sql, NotDestructiveMarker) {
			t.Error("0063 must carry the not-destructive marker and no destructive one")
		}
		if !strings.Contains(m.sql, "ADD COLUMN IF NOT EXISTS own_source_refs JSONB;") {
			t.Error("0063 must add own_source_refs as a NULLABLE column (NULL = a pre-0063 row)")
		}
		return
	}
	t.Fatal("0063_conversation_message_own_sources is not embedded")
}
