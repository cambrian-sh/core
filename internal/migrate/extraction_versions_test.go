package migrate

import (
	"strings"
	"testing"
)

// ADR-0136 Amendment A2.4: 0024 adds the extractor version and passage coverage to
// knowledge_extractions. It is additive, and a marker written before it reads as a
// LEGACY row (version '', coverage NULL) — pending for the backfill.

func TestLoadMigrations_Includes0024ExtractionVersions(t *testing.T) {
	migs, err := loadMigrations(1536)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version != 24 {
			continue
		}
		if m.name != "extraction_versions" {
			t.Fatalf("migration 24 is %q", m.name)
		}
		for _, s := range []string{
			"ADD COLUMN IF NOT EXISTS extractor_version TEXT NOT NULL DEFAULT ''",
			"ADD COLUMN IF NOT EXISTS passages_covered INT",
			"ADD COLUMN IF NOT EXISTS passages_total INT",
		} {
			if !strings.Contains(m.sql, s) {
				t.Errorf("0024 lacks %q", s)
			}
		}
		for _, line := range strings.Split(m.sql, "\n") {
			if l := strings.TrimSpace(line); !strings.HasPrefix(l, "--") && strings.Contains(strings.ToUpper(l), "DROP ") {
				t.Errorf("0024 must be additive: %q", l)
			}
		}
		return
	}
	t.Fatal("0024_extraction_versions is not embedded")
}

// A database at 0022 with a marker row upgrades: the row survives as a legacy marker,
// and a second Migrate is a no-op.
func TestFactItemsMigrations_ExtractionVersionsUpgradeKeepsLegacyMarkers(t *testing.T) {
	ctx, pool := splitTestPool(t)
	freshSchema(ctx, t, pool)
	resetFactTables(ctx, t, pool)
	t.Cleanup(func() { resetFactTables(ctx, t, pool); freshSchema(ctx, t, pool) })

	applyThrough(ctx, t, pool, 22)
	if _, err := pool.Exec(ctx, `INSERT INTO knowledge_extractions (extractor, document_id, content_hash, items)
		VALUES ('memory_fact', 'doc-legacy', 'h', 3)`); err != nil {
		t.Fatalf("seed a 0022 marker: %v", err)
	}
	for range 2 {
		if err := Migrate(ctx, pool, splitTestDim); err != nil {
			t.Fatalf("Migrate over 0022: %v", err)
		}
	}
	var version string
	var covered, total *int
	if err := pool.QueryRow(ctx, `SELECT extractor_version, passages_covered, passages_total FROM knowledge_extractions
		WHERE document_id = 'doc-legacy'`).Scan(&version, &covered, &total); err != nil {
		t.Fatalf("read the legacy marker: %v", err)
	}
	if version != "" || covered != nil || total != nil {
		t.Fatalf("legacy marker = (%q, %v, %v), want ('', NULL, NULL)", version, covered, total)
	}
	if n := count(ctx, t, pool, `SELECT count(*) FROM schema_migrations WHERE version = 24`); n != 1 {
		t.Fatalf("version 24 recorded %d times", n)
	}
}

// T-20261103-fact-backfill-processes-each-principal-s: 0025 adds the two-phase backfill's
// extraction journal. Additive: one new table.
func TestLoadMigrations_Includes0025ExtractionWindows(t *testing.T) {
	migs, err := loadMigrations(1536)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version != 25 {
			continue
		}
		if m.name != "extraction_windows" {
			t.Fatalf("migration 25 is %q", m.name)
		}
		for _, s := range []string{
			"CREATE TABLE IF NOT EXISTS knowledge_extraction_windows",
			"item_ids          TEXT[] NOT NULL",
			"new_item_ids      TEXT[] NOT NULL",
			"PRIMARY KEY (namespace_id, extractor, document_id, content_hash, extractor_version, passage_start)",
		} {
			if !strings.Contains(m.sql, s) {
				t.Errorf("0025 lacks %q", s)
			}
		}
		for _, line := range strings.Split(m.sql, "\n") {
			if l := strings.TrimSpace(line); !strings.HasPrefix(l, "--") && strings.Contains(strings.ToUpper(l), "DROP ") {
				t.Errorf("0025 must be additive: %q", l)
			}
		}
		return
	}
	t.Fatal("0025_extraction_windows is not embedded")
}
