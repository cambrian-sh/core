package migrate

import (
	"strings"
	"testing"
)

// 0064_chunks_text_tsv.sql (ENG-111): a stored generated tsvector column with its
// GIN index, replacing the expression index. A table rewrite, not destructive.
func TestLoadMigrations_Includes0064ChunksTextTSV(t *testing.T) {
	migs, err := loadMigrations(4)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version != 64 {
			continue
		}
		if m.name != "chunks_text_tsv" {
			t.Fatalf("migration 64 is %q", m.name)
		}
		if IsDestructive(m.sql) || !strings.Contains(m.sql, NotDestructiveMarker+" adds a generated column (table rewrite)") {
			t.Error("0064 must carry the owner's not-destructive marker and no destructive one")
		}
		for _, want := range []string{
			"GENERATED ALWAYS AS (to_tsvector('english', text)) STORED",
			"CREATE INDEX IF NOT EXISTS idx_chunks_text_tsv ON chunks USING GIN (text_tsv)",
			"DROP INDEX IF EXISTS idx_chunks_fts",
		} {
			if !strings.Contains(m.sql, want) {
				t.Errorf("0064 lacks %q", want)
			}
		}
		if strings.Index(m.sql, "CREATE INDEX IF NOT EXISTS idx_chunks_text_tsv") > strings.Index(m.sql, "DROP INDEX IF EXISTS idx_chunks_fts") {
			t.Error("the old index is dropped only once the new one exists")
		}
		return
	}
	t.Fatal("0064_chunks_text_tsv is not embedded")
}
