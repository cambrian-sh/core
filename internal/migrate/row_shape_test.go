package migrate

import (
	"fmt"
	"testing"
)

// documentSelectColumns is the SELECT list the pgvector adapter uses for every table that
// can hold a retrievable document (`scanDocument` scans exactly these, in this order).
//
// It is duplicated here deliberately. The adapter and the migrations are two independent
// descriptions of one row shape, and the whole point of this test is to catch them
// disagreeing — importing the list from the adapter would make the test agree with the
// adapter by construction and prove nothing.
var documentSelectColumns = []string{
	"id", "text", "metadata", "access_count", "activation_strength",
	"scoring_prompt_version", "last_accessed_at", "created_at", "document_type",
	"version", "embedding", "summary", "section_path",
}

// retrievableTables must all satisfy that SELECT list, because GetByID/GetBatch/Delete
// consult every one of them for an id that carries no type.
var retrievableTables = []string{"chunks", "tools", "skills", "agent_profiles"}

// This is the regression test for the bug 0008 fixed.
//
// 0007 claimed the descriptor tables kept "the same column set as chunks" and did not check
// it. `section_path` was missing from three tables, and nothing noticed until a live boot
// failed every agent's interview-vector check with `column "section_path" does not exist`.
// Neither the unit tests nor the migration integration tests read a descriptor back through
// the shared SELECT, so the row shape was an unasserted contract between two files.
//
// It runs the actual SELECT rather than comparing column lists from the catalog: a query
// that Postgres accepts is the only proof that matters, and it costs one round trip.
func TestRowShape_EveryRetrievableTableSatisfiesTheDocumentSelect(t *testing.T) {
	ctx, pool := splitTestPool(t)
	freshSchema(ctx, t, pool)
	applyThrough(ctx, t, pool, 8)

	list := ""
	for i, c := range documentSelectColumns {
		if i > 0 {
			list += ", "
		}
		list += `"` + c + `"`
	}

	for _, table := range retrievableTables {
		// LIMIT 0 keeps this about the shape rather than the contents: an empty table is
		// still proof that every column resolves.
		q := fmt.Sprintf("SELECT %s FROM %s LIMIT 0", list, table)
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Errorf("table %q cannot satisfy the adapter's document SELECT: %v\n"+
				"Any column the adapter reads must exist on EVERY table GetByID consults.", table, err)
		}
	}
}

// The document entity is deliberately NOT in that set: it has no embedding, because a full
// document is not a retrieval unit. This asserts the exclusion is real rather than assumed,
// since adding a vector to `documents` would silently put it back in the recall path.
func TestRowShape_DocumentEntityHasNoEmbedding(t *testing.T) {
	ctx, pool := splitTestPool(t)
	freshSchema(ctx, t, pool)
	applyThrough(ctx, t, pool, 8)

	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'documents' AND column_name = 'embedding'
	`).Scan(&n); err != nil {
		t.Fatalf("inspect documents: %v", err)
	}
	if n != 0 {
		t.Error("documents grew an embedding column — a full document is not a retrieval unit, " +
			"and giving it a vector puts it back in the recall index ADR-0093 removed it from")
	}

	// And it must carry the authoritative tags column, which is the reason it exists.
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM documents WHERE tags IS NOT NULL`).Scan(&n); err != nil {
		t.Errorf("documents.tags missing or unusable: %v", err)
	}
}

// Regression for 0009. When 0007 renamed `documents`, PostgreSQL moved
// `document_edges.source_id`'s foreign key along with it, leaving the constraint pointing at
// the frozen legacy copy. Every structural edge written after the split then failed the FK —
// ~700 warnings in a benchmark ingest, non-fatal only because SaveStructuralEdges is
// best-effort, so the structure graph was silently lost while chunks saved fine.
//
// After the split an edge endpoint may be a chunk OR a section, which live in different
// tables, so no single FK can express it. This asserts no such constraint is reintroduced.
func TestRowShape_DocumentEdgesHasNoDanglingForeignKey(t *testing.T) {
	ctx, pool := splitTestPool(t)
	freshSchema(ctx, t, pool)
	// Seeded against the PRE-split schema, then migrated — the corpus has to exist in the
	// shape 0007 expects to find, or the fixture is not exercising the migration at all.
	applyThrough(ctx, t, pool, 6)
	seedLegacyCorpus(ctx, t, pool)
	applyThrough(ctx, t, pool, 9)

	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM information_schema.table_constraints tc
		JOIN information_schema.constraint_column_usage ccu ON tc.constraint_name = ccu.constraint_name
		WHERE tc.constraint_type = 'FOREIGN KEY'
		  AND tc.table_name = 'document_edges'
	`).Scan(&n); err != nil {
		t.Fatalf("inspect document_edges: %v", err)
	}
	if n != 0 {
		t.Errorf("document_edges still carries %d foreign key(s) — an edge endpoint may be a "+
			"chunk or a section, so any single-table FK will reject valid edges", n)
	}

	// And prove an edge between two chunks actually inserts.
	if _, err := pool.Exec(ctx,
		`INSERT INTO document_edges (source_id, target_id, edge_type, weight)
		 VALUES ('doc-a-chunk-1', 'doc-a-chunk-2', 'NEXT', 0.5)
		 ON CONFLICT DO NOTHING`); err != nil {
		t.Errorf("a chunk-to-chunk structural edge was rejected: %v", err)
	}
}
