package postgres

import "testing"

// T-20261103-deleting-a-chunk-leaves-its-chunk-triplets-and: deleting ONE chunk by id —
// the nightly loner decay's path (memory/worker.go → Store.Delete) — must take the
// chunk's KG triplets, PageRank row and every structure/Hebbian edge touching it, in the
// same transaction, and nothing else. Before the fix only the chunk row (and, by FK, its
// chunk_embeddings row) went. Uses the lifecycle test fixtures (PG_TEST_DSN; "lctest-"
// rows, cleaned before and after).
func TestPg_DeleteChunk_TakesItsTripletsPageRankAndEdges(t *testing.T) {
	a, ks, _, ctx := newLifecycleStores(t)
	seedLifecycleDoc(t, a, ks, ctx, "lctest-doc", "agent:a", "text", "d1", 2)
	seedLifecycleDoc(t, a, ks, ctx, "lctest-other", "agent:b", "other", "o1", 2)
	const gone, kept = "lctest-doc-chunk-1", "lctest-doc-chunk-2"
	// A triplet on the surviving chunk, so the test proves the delete is by id and not
	// by document.
	if _, err := a.pool.Exec(ctx, `INSERT INTO chunk_triplets (chunk_id, h, r, t) VALUES ($1, 'bob', 'has', 'car')`, kept); err != nil {
		t.Fatal(err)
	}
	otherEdges := countRows(t, a, ctx, `SELECT count(*) FROM document_edges WHERE source_id LIKE 'lctest-other-%' OR target_id LIKE 'lctest-other-%'`)

	if err := a.Delete(ctx, gone); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	for name, sql := range map[string]string{
		"chunk":     `SELECT count(*) FROM chunks WHERE id = $1`,
		"embedding": `SELECT count(*) FROM chunk_embeddings WHERE chunk_id = $1`,
		"triplets":  `SELECT count(*) FROM chunk_triplets WHERE chunk_id = $1`,
		"pagerank":  `SELECT count(*) FROM chunk_pagerank WHERE chunk_id = $1`,
		"edges":     `SELECT count(*) FROM document_edges WHERE source_id = $1 OR target_id = $1`,
	} {
		if n := countRows(t, a, ctx, sql, gone); n != 0 {
			t.Errorf("deleted chunk still has %d %s row(s)", n, name)
		}
	}
	if n := countRows(t, a, ctx, `SELECT count(*) FROM chunks WHERE id = $1`, kept); n != 1 {
		t.Errorf("the sibling chunk was deleted too: %d", n)
	}
	if n := countRows(t, a, ctx, `SELECT count(*) FROM chunk_triplets WHERE chunk_id = $1`, kept); n != 1 {
		t.Errorf("the sibling chunk's triplet was deleted: %d", n)
	}
	if n := countRows(t, a, ctx, `SELECT count(*) FROM document_sections WHERE document_id = 'lctest-doc'`); n != 1 {
		t.Errorf("a chunk delete must not touch the document's sections: %d", n)
	}
	// The other document lost exactly the Hebbian edge it shared with the deleted chunk.
	if n := countRows(t, a, ctx, `SELECT count(*) FROM document_edges WHERE source_id LIKE 'lctest-other-%' OR target_id LIKE 'lctest-other-%'`); n != otherEdges-1 {
		t.Errorf("unrelated edges: %d -> %d, want %d", otherEdges, n, otherEdges-1)
	}

	// DeleteBatch takes the same side data.
	if err := a.DeleteBatch(ctx, []string{kept}); err != nil {
		t.Fatalf("DeleteBatch: %v", err)
	}
	if n := countRows(t, a, ctx, `SELECT count(*) FROM chunk_triplets WHERE chunk_id = $1`, kept); n != 0 {
		t.Errorf("DeleteBatch left %d triplet(s)", n)
	}
}
