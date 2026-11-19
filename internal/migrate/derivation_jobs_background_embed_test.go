package migrate

import (
	"strings"
	"testing"
)

// 0062_derivation_jobs_background_embed.sql (ENG-2143): the 'embed' derivation kind and
// the staged / chunks_total columns. Not destructive: it widens a CHECK and adds columns.
func TestLoadMigrations_Includes0062BackgroundEmbed(t *testing.T) {
	migs, err := loadMigrations(4)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version != 62 {
			continue
		}
		if m.name != "derivation_jobs_background_embed" {
			t.Fatalf("migration 62 is %q", m.name)
		}
		if IsDestructive(m.sql) {
			t.Error("0062 deletes nothing: it must not carry the destructive marker")
		}
		if !strings.Contains(m.sql, NotDestructiveMarker) {
			t.Error("0062 must carry the not-destructive marker")
		}
		for _, want := range []string{"'embed'", "ADD COLUMN IF NOT EXISTS staged JSONB", "ADD COLUMN IF NOT EXISTS chunks_total"} {
			if !strings.Contains(m.sql, want) {
				t.Errorf("0062 must contain %q", want)
			}
		}
		return
	}
	t.Fatal("0062_derivation_jobs_background_embed is not embedded")
}
