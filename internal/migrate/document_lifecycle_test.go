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

// ADR-0138 Amendment B1.1: 0027 adds the upload association table (additive, keyed by
// its natural key, cascading with its document), and the (writer, digest) duplicate
// lookup index is built concurrently once 0026's columns exist.
func TestLoadMigrations_Includes0027DocumentAssociations(t *testing.T) {
	migs, err := loadMigrations(1536)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	found := false
	for _, m := range migs {
		if m.version != 27 {
			continue
		}
		found = true
		if m.name != "document_associations" {
			t.Fatalf("migration 27 is %q", m.name)
		}
		for _, s := range []string{
			"CREATE TABLE IF NOT EXISTS document_associations",
			"REFERENCES documents(id) ON DELETE CASCADE",
			"PRIMARY KEY (document_id, writer, session_id, scope)",
		} {
			if !strings.Contains(m.sql, s) {
				t.Errorf("0027 lacks %q", s)
			}
		}
		for _, line := range strings.Split(m.sql, "\n") {
			l := strings.ToUpper(strings.TrimSpace(line))
			if strings.HasPrefix(l, "--") {
				continue
			}
			if strings.Contains(l, "DROP ") || strings.Contains(l, "ALTER TABLE") || strings.Contains(l, "CREATE INDEX") {
				t.Errorf("0027 must be a single additive table, found %q", line)
			}
		}
	}
	if !found {
		t.Fatal("migration 0027 missing")
	}
	var ix *concurrentIndex
	for i := range concurrentIndexes {
		if concurrentIndexes[i].name == "idx_documents_writer_digest" {
			ix = &concurrentIndexes[i]
		}
	}
	if ix == nil || ix.table != "documents" || ix.requires != 26 || !strings.Contains(ix.using, "(written_by, content_digest)") {
		t.Fatalf("idx_documents_writer_digest: %+v", ix)
	}
}
