package migrate

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ADR-0136 Amendment A1 migrations: 0021 drops the retired memory_facts tables (A1-Q7:
// 0020 is kept, not rewritten), 0022 adds the substrate pieces. Both must apply on a
// database that already applied 0020 (cambrian_bench's likely state) and on a fresh one,
// and the per-dimension item-embedding HNSW index must be built by the runner's
// companion step.

func TestLoadMigrations_Includes0021And0022FactItems(t *testing.T) {
	migs, err := loadMigrations(1536)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	want := map[int64]string{20: "memory_facts", 21: "drop_memory_facts", 22: "fact_items"}
	for _, m := range migs {
		name, ok := want[m.version]
		if !ok {
			continue
		}
		if m.name != name {
			t.Fatalf("migration %d is %q, want %q", m.version, m.name, name)
		}
		delete(want, m.version)
		switch m.version {
		case 21:
			for _, s := range []string{"DROP TABLE IF EXISTS memory_fact_extractions", "DROP TABLE IF EXISTS memory_facts"} {
				if !strings.Contains(m.sql, s) {
					t.Errorf("0021 lacks %q", s)
				}
			}
		case 22:
			for _, s := range []string{
				"ADD COLUMN IF NOT EXISTS valid_to", "idx_statement_values_entity", "knowledge_assessments",
				"input_item_ids    TEXT[] NOT NULL", "USING gin (input_item_ids)", "predicate_profiles",
				"predicate_profiles_current_uniq", "knowledge_item_embeddings", "REFERENCES knowledge_items(id) ON DELETE CASCADE",
				"knowledge_extractions",
			} {
				if !strings.Contains(m.sql, s) {
					t.Errorf("0022 lacks %q", s)
				}
			}
			if strings.Contains(m.sql, "USING hnsw") {
				t.Error("0022 must not build the HNSW index inside its transaction; the runner's companion step does")
			}
		}
	}
	if len(want) != 0 {
		t.Fatalf("not embedded: %v", want)
	}
	var ix *concurrentIndex
	for _, c := range concurrentIndexesFor(1536) {
		if c.name == ItemEmbeddingIndexName(1536) {
			cc := c
			ix = &cc
		}
	}
	if ix == nil || ix.requires != 22 || !strings.Contains(ix.using, "embedding::vector(1536)") || !strings.Contains(ix.using, "WHERE dims = 1536") {
		t.Fatalf("item embedding index = %+v", ix)
	}
}

func resetFactTables(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS memory_facts, memory_fact_extractions CASCADE`,
		`DROP TABLE IF EXISTS knowledge_assessments, predicate_profiles, knowledge_item_embeddings, knowledge_extractions, knowledge_extraction_windows CASCADE`,
		`DROP INDEX IF EXISTS idx_statement_values_entity`,
		`DO $$ BEGIN IF to_regclass('resolutions') IS NOT NULL THEN ALTER TABLE resolutions DROP COLUMN IF EXISTS valid_to; END IF; END $$`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("reset (%s): %v", stmt, err)
		}
	}
}

func assertFactSchema(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, gone := range []string{"memory_facts", "memory_fact_extractions"} {
		if n := count(ctx, t, pool, `SELECT count(*) FROM pg_class WHERE relname = $1 AND relkind = 'r'`, gone); n != 0 {
			t.Fatalf("%s still exists after 0021", gone)
		}
	}
	for _, tbl := range []string{"knowledge_assessments", "predicate_profiles", "knowledge_item_embeddings", "knowledge_extractions"} {
		if n := count(ctx, t, pool, `SELECT count(*) FROM pg_class WHERE relname = $1 AND relkind = 'r'`, tbl); n != 1 {
			t.Fatalf("%s missing after 0022", tbl)
		}
	}
	if n := count(ctx, t, pool, `SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'resolutions' AND column_name = 'valid_to'`); n != 1 {
		t.Fatal("resolutions.valid_to missing after 0022")
	}
	if n := count(ctx, t, pool, `SELECT count(*) FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		WHERE c.relname = $1 AND i.indisvalid`, ItemEmbeddingIndexName(splitTestDim)); n != 1 {
		t.Fatalf("the per-dimension item embedding HNSW index %s was not built", ItemEmbeddingIndexName(splitTestDim))
	}
	if n := count(ctx, t, pool, `SELECT count(*) FROM schema_migrations WHERE version IN (20, 21, 22)`); n != 3 {
		t.Fatalf("schema_migrations has %d of versions 20-22, want 3", n)
	}
}

// A database that applied 0020 (and wrote a memory_facts row) upgrades cleanly: the
// tables go, the new ones arrive, a second run is a no-op.
func TestFactItemsMigrations_UpgradeFrom0020(t *testing.T) {
	ctx, pool := splitTestPool(t)
	freshSchema(ctx, t, pool)
	resetFactTables(ctx, t, pool)
	t.Cleanup(func() { resetFactTables(ctx, t, pool); freshSchema(ctx, t, pool) })

	applyThrough(ctx, t, pool, 20)
	if _, err := pool.Exec(ctx, `INSERT INTO memory_facts (id, subject, attribute, value, subject_key, attribute_key,
		value_norm, source_chunk_id, source_document_id) VALUES ('fact:x','user','home_city','Paris','user','home_city','paris','c','d')`); err != nil {
		t.Fatalf("seed memory_facts on the 0020 schema: %v", err)
	}
	if err := Migrate(ctx, pool, splitTestDim); err != nil {
		t.Fatalf("Migrate over 0020: %v", err)
	}
	assertFactSchema(ctx, t, pool)
	if err := Migrate(ctx, pool, splitTestDim); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	assertFactSchema(ctx, t, pool)
}

// A fresh database runs 0020, 0021 and 0022 in order with the same result.
func TestFactItemsMigrations_FreshDatabase(t *testing.T) {
	ctx, pool := splitTestPool(t)
	freshSchema(ctx, t, pool)
	resetFactTables(ctx, t, pool)
	t.Cleanup(func() { resetFactTables(ctx, t, pool); freshSchema(ctx, t, pool) })
	if err := Migrate(ctx, pool, splitTestDim); err != nil {
		t.Fatalf("Migrate on a fresh database: %v", err)
	}
	assertFactSchema(ctx, t, pool)
}
