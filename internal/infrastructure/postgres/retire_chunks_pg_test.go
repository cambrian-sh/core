package postgres

import (
	"testing"
)

// ENG-1535: a chunk deleted on its own (the nightly decay) takes the memory_fact
// items keyed into it, with its KG triplets, PageRank and edges, in one
// transaction; the document, its other chunks, and an unrelated document's fact
// stay.
func TestPg_RetireChunks_TakesTheFactItemsKeyedIntoTheChunk(t *testing.T) {
	a, ks, lc, ctx := newLifecycleStores(t)
	seedLifecycleDoc(t, a, ks, ctx, "lctest-rc", "writer-a", "the boat text", "d1", 2)
	seedLifecycleDoc(t, a, ks, ctx, "lctest-other", "writer-a", "other text", "d2", 1)
	c1 := "lctest-rc-chunk-1"
	factsOn := func(chunk string) int {
		return countRows(t, a, ctx, `SELECT count(*) FROM knowledge_items ki JOIN statement_values sv ON sv.item_id = ki.id
			AND sv.predicate = 'source_chunk' WHERE ki.namespace_id = $1 AND sv.value_text = $2`, lcNS, chunk)
	}
	if factsOn(c1) != 1 || factsOn("lctest-other-chunk-1") != 1 {
		t.Fatalf("seed: facts on %s=%d, other=%d", c1, factsOn(c1), factsOn("lctest-other-chunk-1"))
	}
	counts, err := lc.RetireChunks(ctx, []string{c1})
	if err != nil {
		t.Fatalf("RetireChunks: %v", err)
	}
	if counts[cascadeFactItems] != 1 || counts[cascadeChunks] != 1 {
		t.Fatalf("counts %v, want 1 fact item and 1 chunk", counts)
	}
	if factsOn(c1) != 0 {
		t.Fatal("the fact item keyed into the retired chunk survived")
	}
	for name, n := range map[string]int{
		"chunk":    countRows(t, a, ctx, `SELECT count(*) FROM chunks WHERE id = $1`, c1),
		"triplets": countRows(t, a, ctx, `SELECT count(*) FROM chunk_triplets WHERE chunk_id = $1`, c1),
		"pagerank": countRows(t, a, ctx, `SELECT count(*) FROM chunk_pagerank WHERE chunk_id = $1`, c1),
		"edges":    countRows(t, a, ctx, `SELECT count(*) FROM document_edges WHERE source_id = $1 OR target_id = $1`, c1),
	} {
		if n != 0 {
			t.Errorf("%s of the retired chunk left: %d", name, n)
		}
	}
	if n := countRows(t, a, ctx, `SELECT count(*) FROM chunks WHERE id = 'lctest-rc-chunk-2'`); n != 1 {
		t.Error("the document's other chunk was deleted")
	}
	if n := countRows(t, a, ctx, `SELECT count(*) FROM documents WHERE id = 'lctest-rc'`); n != 1 {
		t.Error("the document row was deleted")
	}
	if factsOn("lctest-other-chunk-1") != 1 {
		t.Error("an unrelated document's fact item was erased")
	}
	if c, err := lc.RetireChunks(ctx, nil); err != nil || len(c) != 0 {
		t.Fatalf("empty retire: %v %v", c, err)
	}
}
