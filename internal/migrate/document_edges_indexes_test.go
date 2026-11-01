package migrate

import (
	"testing"
)

// Tests for 0018_document_edges_indexes.sql
// (T-20261031-document-edges-has-no-source-id-target-id-index).

const edgeIndexMigrationVersion = 18

var documentEdgeIndexes = []string{"idx_doc_edges_source", "idx_doc_edges_target"}

func TestLoadMigrations_Includes0018DocumentEdgesIndexes(t *testing.T) {
	migs, err := loadMigrations(4)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version == edgeIndexMigrationVersion {
			if m.name != "document_edges_indexes" {
				t.Fatalf("migration 18 is %q, want document_edges_indexes", m.name)
			}
			return
		}
	}
	t.Fatal("migration 0018_document_edges_indexes.sql is not embedded")
}

// Through the real runner, on a fresh database: both indexes exist afterwards, the
// migration is recorded once, and replaying the file on a database that already has
// them is a no-op (the legacy db/migrations/006 created the same names).
func TestDocumentEdgesIndexes_CreatedAndIdempotent(t *testing.T) {
	ctx, pool := splitTestPool(t)
	freshSchema(ctx, t, pool)
	t.Cleanup(func() { freshSchema(ctx, t, pool) })

	if err := Migrate(ctx, pool, splitTestDim); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	for _, idx := range documentEdgeIndexes {
		if n := count(ctx, t, pool,
			`SELECT count(*) FROM pg_indexes WHERE tablename = 'document_edges' AND indexname = $1`, idx); n != 1 {
			t.Fatalf("after 0018, index %s on document_edges: found %d, want 1", idx, n)
		}
	}
	if n := count(ctx, t, pool, `SELECT count(*) FROM schema_migrations WHERE version = $1`,
		edgeIndexMigrationVersion); n != 1 {
		t.Fatalf("schema_migrations has %d rows for version 18, want 1", n)
	}

	migs, err := loadMigrations(splitTestDim)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version == edgeIndexMigrationVersion {
			if _, err := pool.Exec(ctx, m.sql); err != nil {
				t.Fatalf("replaying 0018 on a database that already has the indexes: %v", err)
			}
		}
	}
	if err := Migrate(ctx, pool, splitTestDim); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}
