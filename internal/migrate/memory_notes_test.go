package migrate

import (
	"strings"
	"testing"
)

// 0030_memory_notes.sql (ADR-0139 D14): additive only — three new tables, nothing
// altered or dropped, so older binaries keep working on a migrated database.
func TestLoadMigrations_Includes0030MemoryNotes(t *testing.T) {
	migs, err := loadMigrations(4)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version != 30 {
			continue
		}
		if m.name != "memory_notes" {
			t.Fatalf("migration 30 is %q, want memory_notes", m.name)
		}
		for _, s := range []string{
			"CREATE TABLE IF NOT EXISTS memory_notes (",
			"CREATE TABLE IF NOT EXISTS memory_notes_budgets (",
			"CREATE TABLE IF NOT EXISTS memory_notes_history (",
			"PRIMARY KEY (scope, owner_id, revision)",
		} {
			if !strings.Contains(m.sql, s) {
				t.Errorf("0030 lacks %q", s)
			}
		}
		for _, line := range strings.Split(m.sql, "\n") {
			l := strings.ToUpper(strings.TrimSpace(line))
			if strings.HasPrefix(l, "--") {
				continue
			}
			if strings.Contains(l, "DROP ") || strings.Contains(l, "ALTER ") || strings.Contains(l, "TRUNCATE") {
				t.Errorf("0030 must be additive: %q", line)
			}
		}
		return
	}
	t.Fatal("0030_memory_notes is not embedded")
}
