package migrate

import (
	"strings"
	"testing"
)

// 0051_memory_erasure.sql (ADR-0137 S1, ENG-411 stage A): erasure state tables, the
// derivative FKs after an orphan sweep, and the evidence FK actions. Marked destructive
// because the sweep deletes orphan rows.
func TestLoadMigrations_Includes0051MemoryErasure(t *testing.T) {
	migs, err := loadMigrations(4)
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	for _, m := range migs {
		if m.version != 51 {
			continue
		}
		if m.name != "memory_erasure" {
			t.Fatalf("migration 51 is %q", m.name)
		}
		if !m.destructive {
			t.Error("0051 deletes orphan rows: it must carry the destructive marker")
		}
		for _, s := range []string{
			"CREATE TABLE IF NOT EXISTS memory_erasures",
			"CREATE TABLE IF NOT EXISTS memory_erasure_targets",
			"CREATE TABLE IF NOT EXISTS memory_erasure_outbox",
			"FOREIGN KEY (chunk_id) REFERENCES chunks(id) ON DELETE CASCADE",
			"FOREIGN KEY (evidence_id) REFERENCES evidence(id) ON DELETE CASCADE",
			"FOREIGN KEY (revises_id) REFERENCES evidence(id) ON DELETE SET NULL",
			"FOREIGN KEY (first_seen_evidence) REFERENCES evidence(id) ON DELETE SET NULL",
		} {
			if !strings.Contains(m.sql, s) {
				t.Errorf("0051 lacks %q", s)
			}
		}
		// The tombstone is content-free: no column that could hold text, a vector or a value.
		for _, line := range strings.Split(m.sql, "\n") {
			l := strings.ToLower(strings.TrimSpace(line))
			if strings.HasPrefix(l, "--") {
				continue
			}
			for _, bad := range []string{"vector(", " text_body", "quote ", "value_text", " body "} {
				if strings.Contains(l, bad) {
					t.Errorf("0051 must stay content-free, found %q in %q", bad, line)
				}
			}
		}
		return
	}
	t.Fatal("0051_memory_erasure is not embedded")
}

// On a database that already holds data — including orphan triplet/PageRank rows left by
// chunk deletes — 0051 removes ONLY the orphans, adds the FKs, and leaves every live row.
func TestMemoryErasure0051_OrphanSweepAndFKs_Pg(t *testing.T) {
	ctx, pool, _ := guardPool(t)
	applyThrough(ctx, t, pool, 50)
	vec := "[0.1,0.2,0.3,0.4]"
	for _, q := range []string{
		`INSERT INTO documents (id, title, text) VALUES ('mig-doc', 'D', 'body')`,
		`INSERT INTO chunks (id, document_id, text, embedding) VALUES ('mig-c1', 'mig-doc', 'one', '` + vec + `')`,
		`INSERT INTO chunks (id, document_id, text, embedding) VALUES ('mig-c2', 'mig-doc', 'two', '` + vec + `')`,
		`INSERT INTO chunk_triplets (chunk_id, h, r, t) VALUES ('mig-c1', 'a', 'r', 'b')`,
		`INSERT INTO chunk_triplets (chunk_id, h, r, t) VALUES ('mig-gone', 'x', 'r', 'y')`,
		`INSERT INTO chunk_pagerank (chunk_id, score) VALUES ('mig-c2', 0.4)`,
		`INSERT INTO chunk_pagerank (chunk_id, score) VALUES ('mig-gone', 0.9)`,
		`INSERT INTO evidence (id, source_id, source_key, source_revision, content_hash) VALUES ('ev-1', 's', 'k', '1', 'h1')`,
		`INSERT INTO evidence (id, source_id, source_key, source_revision, content_hash, revises_id) VALUES ('ev-2', 's', 'k', '2', 'h2', 'ev-1')`,
		`INSERT INTO evidence_outbox (evidence_id) VALUES ('ev-1')`,
		`INSERT INTO entities (id, kind, first_seen_evidence) VALUES ('customer/c1', 'customer', 'ev-1')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}

	applyThrough(ctx, t, pool, 51)

	if n := count(ctx, t, pool, `SELECT count(*) FROM chunk_triplets WHERE chunk_id = 'mig-gone'`) +
		count(ctx, t, pool, `SELECT count(*) FROM chunk_pagerank WHERE chunk_id = 'mig-gone'`); n != 0 {
		t.Fatalf("orphans survived: %d", n)
	}
	if n := count(ctx, t, pool, `SELECT count(*) FROM chunk_triplets`) + count(ctx, t, pool, `SELECT count(*) FROM chunk_pagerank`); n != 2 {
		t.Fatalf("live derivative rows = %d, want 2 (the sweep must not touch rows whose chunk exists)", n)
	}
	for _, tbl := range []string{"memory_erasures", "memory_erasure_targets", "memory_erasure_outbox"} {
		if !relationExists(ctx, t, pool, tbl) {
			t.Errorf("%s missing", tbl)
		}
	}

	// FK: an in-flight triplet for a chunk that does not exist is refused (no resurrection).
	if _, err := pool.Exec(ctx, `INSERT INTO chunk_triplets (chunk_id, h, r, t) VALUES ('mig-nope', 'a', 'r', 'b')`); err == nil {
		t.Fatal("triplet for a missing chunk was accepted")
	}
	// Cascade: deleting a chunk takes its triplets and PageRank row.
	if _, err := pool.Exec(ctx, `DELETE FROM chunks WHERE id IN ('mig-c1', 'mig-c2')`); err != nil {
		t.Fatal(err)
	}
	if n := count(ctx, t, pool, `SELECT count(*) FROM chunk_triplets`) + count(ctx, t, pool, `SELECT count(*) FROM chunk_pagerank`); n != 0 {
		t.Fatalf("chunk delete left %d derivative rows", n)
	}
	// Evidence FK actions: outbox cascades, revises_id and first_seen_evidence become NULL.
	if _, err := pool.Exec(ctx, `DELETE FROM evidence WHERE id = 'ev-1'`); err != nil {
		t.Fatalf("evidence delete with outbox/revision/entity dependents: %v", err)
	}
	if n := count(ctx, t, pool, `SELECT count(*) FROM evidence_outbox`); n != 0 {
		t.Errorf("outbox rows survived their evidence: %d", n)
	}
	if n := count(ctx, t, pool, `SELECT count(*) FROM evidence WHERE id = 'ev-2' AND revises_id IS NULL`); n != 1 {
		t.Error("revises_id was not set NULL")
	}
	if n := count(ctx, t, pool, `SELECT count(*) FROM entities WHERE id = 'customer/c1' AND first_seen_evidence IS NULL`); n != 1 {
		t.Error("entities.first_seen_evidence was not set NULL (the entity handle must survive)")
	}
	// knowledge_items / events / links keep NO ACTION: a forgotten dependent is an error.
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, event_type, occurred_at, evidence_id) VALUES ('evt-1', 't', now(), 'ev-2')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM evidence WHERE id = 'ev-2'`); err == nil {
		t.Error("deleting evidence an event still cites must fail (NO ACTION), not cascade silently")
	}

	// Idempotent: the file applies again on the migrated schema without error.
	migs, err := loadMigrations(splitTestDim)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migs {
		if m.version == 51 {
			if _, err := pool.Exec(ctx, m.sql); err != nil {
				t.Fatalf("0051 is not idempotent: %v", err)
			}
		}
	}
}
