package migrate

import (
	"strings"
	"testing"
)

// 0046_action_records.sql (ADR-0154): additive, one table with the lifecycle
// vocabulary pinned by a CHECK.
func TestLoadMigrations_Includes0046ActionRecords(t *testing.T) {
	migs, err := loadMigrations(4)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version != 46 {
			continue
		}
		if m.name != "action_records" {
			t.Fatalf("migration 46 is %q", m.name)
		}
		for _, s := range []string{"CREATE TABLE IF NOT EXISTS action_records", "key         TEXT PRIMARY KEY",
			"'proposed','approved','dispatched','accepted','confirmed','not_confirmed'"} {
			if !strings.Contains(m.sql, s) {
				t.Errorf("0046 lacks %q", s)
			}
		}
		for _, line := range strings.Split(m.sql, "\n") {
			l := strings.ToUpper(strings.TrimSpace(line))
			if !strings.HasPrefix(l, "--") && (strings.Contains(l, "DROP ") || strings.Contains(l, "DELETE ")) {
				t.Errorf("0046 must be additive: %q", line)
			}
		}
		return
	}
	t.Fatal("0046_action_records is not embedded")
}
