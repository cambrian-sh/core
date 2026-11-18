package migrate

import (
	"strings"
	"testing"
)

// 0056_links_supporting_evidence.sql (ENG-2032 with ENG-2030): links record every
// evidence row they were derived from, for the ADR-0137 erasure cascade. Additive.
func TestLoadMigrations_Includes0056LinksSupportingEvidence(t *testing.T) {
	migs, err := loadMigrations(4)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version != 56 {
			continue
		}
		if m.name != "links_supporting_evidence" {
			t.Fatalf("migration 56 is %q", m.name)
		}
		if IsDestructive(m.sql) {
			t.Error("0056 deletes and rewrites nothing: it must not carry the destructive marker")
		}
		if !strings.Contains(m.sql, NotDestructiveMarker) {
			t.Error("0056 must carry the not-destructive marker (every migration from 0051 on does)")
		}
		if !strings.Contains(m.sql, "supporting_evidence TEXT[] NOT NULL DEFAULT '{}'") {
			t.Error("0056 must add links.supporting_evidence with a constant default (no backfill)")
		}
		for _, line := range strings.Split(m.sql, "\n") {
			l := strings.ToUpper(strings.TrimSpace(line))
			if strings.HasPrefix(l, "--") {
				continue
			}
			if strings.Contains(l, "DELETE ") || strings.Contains(l, "UPDATE ") || strings.Contains(l, "DROP TABLE") || strings.Contains(l, "DROP COLUMN") {
				t.Errorf("0056 must not delete or rewrite rows: %q", line)
			}
		}
		return
	}
	t.Fatal("0056_links_supporting_evidence is not embedded")
}
