package postgres

import (
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// a1DocFact is a1Fact recording its source document (FactPredSourceDocument), as the
// extractor does for every fact it writes.
func a1DocFact(pred, value, chunk, doc string, tags ...string) domain.KnowledgeItem {
	it := a1Fact(pred, value, chunk, a1Day("2023-01-01"), tags...)
	f := domain.FactFromItem(it)
	f.SourceDocumentID = doc
	f.Tags = tags
	return f.ToItem(a1NS)
}

// ADR-0136 A4 on the store: SourceDocumentIDs restricts the fact search, in SQL, to the
// facts of those documents — an EXACT ranking over them, so a fact many nearer facts of
// other documents would push out of a global top-k is returned — and the classification
// predicate still applies in full. The lookup index the restriction uses exists.
func TestPg_FactItems_ScopedToSourceDocuments(t *testing.T) {
	ks, r, ctx := newFactSubstrate(t)
	evidence := a1DocFact("planted_saplings", "12 tomato saplings", "a1-d1-c1", "a1test-doc-1", "public")
	other := a1DocFact("garden_size", "small", "a1-d2-c4", "a1test-doc-2", "public")
	secret := a1DocFact("diary", "hidden", "a1-d1-c2", "a1test-doc-1", "secret")
	outside := a1DocFact("pest", "aphids", "a1-d3-c1", "a1test-doc-3", "public")
	items := []domain.KnowledgeItem{evidence, other, secret, outside}
	embs := []domain.ItemEmbedding{
		{ItemID: evidence.ID, ModelID: "a1-model", Vector: unitVec(9)},
		{ItemID: other.ID, ModelID: "a1-model", Vector: unitVec(8)},
		{ItemID: secret.ID, ModelID: "a1-model", Vector: unitVec(1)},
		{ItemID: outside.ID, ModelID: "a1-model", Vector: unitVec(1)},
	}
	// 30 facts of 30 other documents, every one nearer to the query than the evidence.
	for i := 0; i < 30; i++ {
		n := a1DocFact("noise_"+string(rune('a'+i%26))+string(rune('a'+i/26)), "v", "a1-n-c", "a1test-noise-"+string(rune('a'+i)), "public")
		items = append(items, n)
		v := unitVec(1)
		v[9] = 0.01 // cosine to unitVec(1) ~ 1, to the evidence direction ~ 0
		embs = append(embs, domain.ItemEmbedding{ItemID: n.ID, ModelID: "a1-model", Vector: v})
	}
	putAll(t, ks, ctx, items...)
	if err := ks.PutItemEmbeddings(ctx, embs); err != nil {
		t.Fatal(err)
	}
	pub := &domain.TagPredicate{ForbiddenTags: []string{"secret"}}
	q := unitVec(1)
	q[9] = 0.5 // nearest to the noise and to `outside`, the evidence a distant second

	global, err := r.SearchFacts(ctx, q, domain.FactSearchOptions{TopK: 20, Scope: pub})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range global {
		if h.Fact.ID == string(evidence.ID) {
			t.Fatal("precondition: the global top-20 must not hold the evidence fact")
		}
	}

	scoped, err := r.SearchFacts(ctx, q, domain.FactSearchOptions{TopK: 20, Scope: pub,
		SourceDocumentIDs: []string{"a1test-doc-1", "a1test-doc-2"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 2 || scoped[0].Fact.ID != string(evidence.ID) || scoped[1].Fact.ID != string(other.ID) {
		t.Fatalf("scoped hits = %+v, want the evidence then the doc-2 fact (no secret, nothing of doc-3)", scoped)
	}
	if scoped[0].Fact.SourceChunkID != "a1-d1-c1" || scoped[0].Fact.SourceDocumentID != "a1test-doc-1" || scoped[0].Score <= scoped[1].Score {
		t.Fatalf("scoped hit view = %+v", scoped[0])
	}
	if one, _ := r.SearchFacts(ctx, q, domain.FactSearchOptions{TopK: 1, Scope: pub, SourceDocumentIDs: []string{"a1test-doc-1", "a1test-doc-2"}}); len(one) != 1 || one[0].Fact.ID != string(evidence.ID) {
		t.Fatalf("top_k 1 = %+v", one)
	}
	// With the bypass the secret fact of doc-1 is returned (the restriction, not the
	// predicate, is what kept doc-3 out above).
	all, _ := r.SearchFacts(ctx, q, domain.FactSearchOptions{TopK: 20, Scope: &domain.TagPredicate{Bypass: true}, SourceDocumentIDs: []string{"a1test-doc-1"}})
	if len(all) != 2 || all[0].Fact.ID != string(secret.ID) {
		t.Fatalf("bypass scoped = %+v, want the secret fact first, then the evidence", all)
	}
	if none, err := r.SearchFacts(ctx, q, domain.FactSearchOptions{TopK: 20, Scope: pub, SourceDocumentIDs: []string{"a1test-no-such-doc"}}); err != nil || len(none) != 0 {
		t.Fatalf("unknown document: %v %v", none, err)
	}

	var def string
	if err := ks.pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE indexname = 'idx_statement_values_source_document'`).Scan(&def); err != nil {
		t.Fatalf("idx_statement_values_source_document missing (run `orchestrator migrate up` with this binary): %v", err)
	}
	if !strings.Contains(def, "value_text") || !strings.Contains(def, "source_document") {
		t.Fatalf("index definition = %s", def)
	}
}
