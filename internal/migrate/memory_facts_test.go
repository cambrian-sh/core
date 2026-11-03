package migrate

import (
	"strings"
	"testing"
)

// ADR-0136 D4/D5: 0020 is embedded, sized from ${EMBEDDING_DIM}, and carries the
// open-interval EXCLUDE constraint over btree_gist (owner-assumed Q7) and the indexes
// erasure needs (source chunk / source document).
func TestLoadMigrations_Includes0020MemoryFacts(t *testing.T) {
	migs, err := loadMigrations(1536)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version != 20 {
			continue
		}
		if m.name != "memory_facts" {
			t.Fatalf("migration 20 is %q, want memory_facts", m.name)
		}
		for _, want := range []string{
			"CREATE EXTENSION IF NOT EXISTS btree_gist",
			"vector(1536)",
			"EXCLUDE USING gist",
			"WHERE (cardinality = 'single' AND retracted_at IS NULL AND corroborates IS NULL)",
			"idx_memory_facts_source_chunk",
			"idx_memory_facts_source_document",
			"USING hnsw (embedding vector_cosine_ops)",
			"memory_fact_extractions",
		} {
			if !strings.Contains(m.sql, want) {
				t.Errorf("0020 lacks %q", want)
			}
		}
		return
	}
	t.Fatal("migration 0020_memory_facts.sql is not embedded")
}
