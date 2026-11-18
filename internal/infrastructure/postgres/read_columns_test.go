package postgres

import (
	"strings"
	"testing"
)

// readColumns: under ADR-0107 projection read a chunks read hydrates `embedding`
// from chunk_embeddings for the active model; legacy mode and the descriptor
// tables (no projection, ENG-2038) keep the column
// (T-20261118-under-adr-0107-projection-read-by-id-reads).
func TestReadColumns_ProjectionReadHydratesActiveModelForChunksOnly(t *testing.T) {
	sqlFor := func(p *PgVectorAdapter, table string) string {
		t.Helper()
		sql, _, err := dialect.From(table).Select(p.readColumns(table)...).ToSQL()
		if err != nil {
			t.Fatal(err)
		}
		return sql
	}
	proj := &PgVectorAdapter{dim: 1536, projectionRead: true, projModelID: "m2"}
	got := sqlFor(proj, TableChunks)
	if !strings.Contains(got, "(SELECT ce.embedding::vector(1536) FROM chunk_embeddings ce WHERE ce.chunk_id = chunks.id AND ce.model_id = 'm2') AS \"embedding\"") {
		t.Fatalf("projection-read chunks columns = %s, want the active-model subquery as embedding", got)
	}
	if strings.Contains(got, `"version", "embedding"`) {
		t.Fatalf("projection-read chunks read still selects the legacy column: %s", got)
	}
	for _, table := range []string{TableTools, TableSkills, TableAgentProfiles} {
		if s := sqlFor(proj, table); strings.Contains(s, "chunk_embeddings") {
			t.Errorf("%s read joins the chunk projection: %s", table, s)
		}
	}
	legacy := &PgVectorAdapter{dim: 1024}
	if s := sqlFor(legacy, TableChunks); strings.Contains(s, "chunk_embeddings") || !strings.Contains(s, `"embedding"`) {
		t.Errorf("legacy chunks read changed: %s", s)
	}
}
