package migrate

import (
	"strings"
	"testing"
)

// 0054_typed_row_classification.sql (ADR-0158 D1, D7; ENG-1974): four additive,
// catalog-only columns, not destructive, no backfill.
func TestLoadMigrations_Includes0054TypedRowClassification(t *testing.T) {
	migs, err := loadMigrations(4)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version != 54 {
			continue
		}
		if m.name != "typed_row_classification" {
			t.Fatalf("migration 54 is %q", m.name)
		}
		if IsDestructive(m.sql) {
			t.Error("0054 only adds columns: it must not carry the destructive marker")
		}
		if !strings.Contains(m.sql, NotDestructiveMarker) {
			t.Error("0054 must carry the not-destructive marker (every migration from 0051 on does)")
		}
		for _, s := range []string{
			"ALTER TABLE events       ADD COLUMN IF NOT EXISTS classification TEXT[] NOT NULL DEFAULT '{}'",
			"ALTER TABLE observations ADD COLUMN IF NOT EXISTS classification TEXT[] NOT NULL DEFAULT '{}'",
			"ALTER TABLE links        ADD COLUMN IF NOT EXISTS classification TEXT[] NOT NULL DEFAULT '{}'",
			"ALTER TABLE entities     ADD COLUMN IF NOT EXISTS first_seen_classification TEXT[] NOT NULL DEFAULT '{}'",
		} {
			if !strings.Contains(m.sql, s) {
				t.Errorf("0054 lacks %q", s)
			}
		}
		for _, line := range strings.Split(m.sql, "\n") {
			l := strings.ToUpper(strings.TrimSpace(line))
			if !strings.HasPrefix(l, "--") && (strings.Contains(l, "DROP ") || strings.Contains(l, "DELETE ") || strings.Contains(l, "UPDATE ")) {
				t.Errorf("0054 must be additive with no backfill: %q", line)
			}
		}
		return
	}
	t.Fatal("0054_typed_row_classification is not embedded")
}

// On a database that already holds typed rows, 0054 leaves every row in place and
// reads its new column as '{}' (ADR-0158 D7: R = ∅ means visibility is unchanged),
// including rows in the observations partitions; the columns refuse NULL.
func TestTypedRowClassification0054_ExistingRowsReadEmpty_Pg(t *testing.T) {
	ctx, pool, _ := guardPool(t)
	applyThrough(ctx, t, pool, 53)
	for _, q := range []string{
		`INSERT INTO evidence (id, source_id, source_key, source_revision, content_hash) VALUES ('ev-1', 's', 'k', '1', 'h1')`,
		`INSERT INTO events (id, event_type, occurred_at, evidence_id, source_ref) VALUES ('evt-1', 'transfer', now(), 'ev-1', 'r1')`,
		`INSERT INTO observations (entity_id, predicate, value_type, value_text, occurred_at, evidence_id, source_ref)
			VALUES ('customer/c1', 'status', 'text', 'gold', now(), 'ev-1', 'r1')`,
		`INSERT INTO entities (id, kind, first_seen_evidence) VALUES ('customer/c1', 'customer', 'ev-1')`,
		`INSERT INTO links (id, family, from_ref, to_ref, relation, state, mechanism, evidence_id, asserted_by, asserted_at)
			VALUES ('lnk-1', 'relation', 'entity:customer/c1', 'entity:company/a', 'customer_of', 'candidate', 'declared', 'ev-1', 'p', now())`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}

	applyThrough(ctx, t, pool, 54)

	for _, q := range []string{
		`SELECT count(*) FROM events WHERE id = 'evt-1' AND classification = '{}'`,
		`SELECT count(*) FROM observations WHERE entity_id = 'customer/c1' AND classification = '{}'`,
		`SELECT count(*) FROM observations_default WHERE entity_id = 'customer/c1' AND classification = '{}'`,
		`SELECT count(*) FROM links WHERE id = 'lnk-1' AND classification = '{}'`,
		`SELECT count(*) FROM entities WHERE id = 'customer/c1' AND first_seen_classification = '{}'`,
	} {
		if n := count(ctx, t, pool, q); n != 1 {
			t.Errorf("%s = %d, want 1 (existing rows keep and read an empty row classification)", q, n)
		}
	}
	for _, q := range []string{
		`UPDATE events SET classification = NULL`,
		`UPDATE observations SET classification = NULL`,
		`UPDATE links SET classification = NULL`,
		`UPDATE entities SET first_seen_classification = NULL`,
	} {
		if _, err := pool.Exec(ctx, q); err == nil {
			t.Errorf("%s was accepted: the column must be NOT NULL", q)
		}
	}
	// Re-running is a no-op (IF NOT EXISTS).
	migs, err := loadMigrations(splitTestDim)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migs {
		if m.version == 54 {
			if _, err := pool.Exec(ctx, m.sql); err != nil {
				t.Fatalf("0054 is not idempotent: %v", err)
			}
		}
	}
}
