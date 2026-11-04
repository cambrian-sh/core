package memory

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// ADR-0136 A4: the document-scoped fact lane.

// rankedChunkStore is laneChunkStore whose dense Search returns the listed chunk ids in
// that rank order (descending score), under the read predicate.
type rankedChunkStore struct {
	*laneChunkStore
	dense []string
}

func (s *rankedChunkStore) Search(_ context.Context, _ []float32, opts domain.SearchOptions) ([]domain.SearchResult, error) {
	byID := map[string]domain.Document{}
	for _, d := range s.docs {
		byID[d.ID] = d
	}
	var out []domain.SearchResult
	for i, id := range s.dense {
		d, ok := byID[id]
		if !ok || (opts.Scope != nil && !opts.Scope.Allows(docTags(d.Metadata))) {
			continue
		}
		sc := 0.9 - float64(i)*0.01
		out = append(out, domain.SearchResult{Document: d, Score: sc, RawScore: sc})
	}
	return out, nil
}

// scoredFactReader is a domain.FactItemReader whose facts carry a fixed cosine to the
// query, so "outranked globally" can be stated exactly. It honours Scope (the fact's
// tags), SourceDocumentIDs and TopK like PgFactItemReader, and records every search.
type scoredFactReader struct {
	mu    sync.Mutex
	facts []domain.FactHit
	calls []domain.FactSearchOptions
}

func (r *scoredFactReader) SearchFacts(_ context.Context, _ []float32, opts domain.FactSearchOptions) ([]domain.FactHit, error) {
	r.mu.Lock()
	r.calls = append(r.calls, opts)
	r.mu.Unlock()
	in := map[string]bool{}
	for _, d := range opts.SourceDocumentIDs {
		in[d] = true
	}
	var out []domain.FactHit
	for _, h := range r.facts {
		if opts.Scope != nil && !opts.Scope.Bypass && !opts.Scope.Allows(h.Fact.Tags) {
			continue
		}
		if len(in) > 0 && !in[h.Fact.SourceDocumentID] {
			continue
		}
		out = append(out, h)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if opts.TopK > 0 && len(out) > opts.TopK {
		out = out[:opts.TopK]
	}
	return out, nil
}

func (r *scoredFactReader) FactChain(context.Context, domain.Fact, *domain.TagPredicate) ([]domain.Fact, error) {
	return nil, nil
}

func (r *scoredFactReader) SimilarFacts(context.Context, domain.Fact, []float32, int, *domain.TagPredicate) ([]domain.Fact, error) {
	return nil, nil
}

func (r *scoredFactReader) scopedCalls() []domain.FactSearchOptions {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.FactSearchOptions
	for _, c := range r.calls {
		if len(c.SourceDocumentIDs) > 0 {
			out = append(out, c)
		}
	}
	return out
}

// chunkOf is a chunk of document docID (its ingest parent in metadata, as
// ingestion_chunks.go stamps it).
func chunkOf(id, docID string, tags ...string) domain.Document {
	return domain.Document{ID: id, Text: "text of " + id, DocumentType: domain.DocTypeMnemonicFact,
		Metadata: map[string]any{"tags": tags, "document_id": docID}}
}

func scoredFact(id, chunkID, docID, value string, score float64, tags ...string) domain.FactHit {
	f := laneFact(chunkID, value, tags...)
	f.ID = id
	f.SourceDocumentID = docID
	f.Attribute, f.AttributeKey = "detail_"+id, "detail_"+id
	return domain.FactHit{Fact: f, Score: score}
}

func scopedLaneService(t *testing.T, store *rankedChunkStore, reader domain.FactItemReader, pred *domain.TagPredicate, n int) *QueryService {
	t.Helper()
	read := authz.NewEnforcingVectorStore(store, quietLog())
	q := NewQueryServiceWithFacts(&fakeEmbedder{}, read, &policyAuthorizer{
		known: map[string]bool{"agent": true},
		preds: map[string]*domain.TagPredicate{"agent": pred},
	}, authz.NewEnforcingFactItemReader(reader, quietLog()))
	q.SetRankingV2(RankingV2{FactLane: true, FactMatchWeight: 0.2, FactLaneTopK: 20,
		FactLaneScopedDocuments: n, FactLaneScopedTopK: 20})
	return q
}

// The A4 case: the evidence fact (doc-B, cosine 0.50) is outranked by 25 higher-cosine
// facts of OTHER documents, so the global top-20 never holds it. Its document is the
// second distinct document of the pool, so with N=2 the scoped lane finds it and its
// source chunk joins the results, annotated; a fact of doc-C (the third document, outside
// N) is not added, although its cosine (0.55) is higher than the evidence fact's.
func TestFactLaneScoped_FindsAGloballyOutrankedFactInTopNDocuments(t *testing.T) {
	t.Parallel()
	chunks := []domain.Document{
		chunkOf("a-0", "doc-A", "t"), chunkOf("a-1", "doc-A", "t"),
		chunkOf("b-0", "doc-B", "t"), chunkOf("b-7", "doc-B", "t"),
		chunkOf("c-0", "doc-C", "t"), chunkOf("c-5", "doc-C", "t"),
	}
	// Dense rank order: two chunks of doc-A first, then doc-B, then doc-C.
	store := &rankedChunkStore{laneChunkStore: &laneChunkStore{docs: chunks}, dense: []string{"a-0", "a-1", "b-0", "c-0"}}
	reader := &scoredFactReader{}
	for i := 0; i < 25; i++ {
		// Facts of documents the principal has but the pool does not hold; their chunks
		// are not in the store, so the global lane brings nothing in for them.
		id := "x" + string(rune('a'+i))
		reader.facts = append(reader.facts, scoredFact(id, "x-"+id, "doc-X"+id, "noise", 0.9-float64(i)*0.001, "t"))
	}
	reader.facts = append(reader.facts,
		scoredFact("evidence", "b-7", "doc-B", "planted 12 tomato saplings", 0.50, "t"),
		scoredFact("outside", "c-5", "doc-C", "outside the top N", 0.55, "t"))

	run := func(n int) map[string]domain.SearchResult {
		q := scopedLaneService(t, store, reader, &domain.TagPredicate{}, n)
		res, err := q.Search(context.Background(), "when did I plant the saplings", "agent")
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]domain.SearchResult{}
		for _, r := range res {
			out[r.Document.ID] = r
		}
		return out
	}

	off := run(0)
	if _, ok := off["b-7"]; ok {
		t.Fatal("precondition: the global lane alone must not reach the outranked fact's chunk")
	}
	if len(reader.scopedCalls()) != 0 {
		t.Fatalf("N=0 must run no scoped search: %+v", reader.scopedCalls())
	}

	on := run(2)
	b7, ok := on["b-7"]
	if !ok {
		t.Fatalf("the scoped lane must bring the evidence chunk in: %v", keys(on))
	}
	if lines := factLines(b7); len(lines) != 1 || !strings.Contains(lines[0], "planted 12 tomato saplings") {
		t.Fatalf("the evidence chunk must carry its fact text: %v", lines)
	}
	if s, _ := b7.Document.Metadata[domain.MetaFactMatchScore].(float64); s != 0.50 {
		t.Fatalf("fact_match = %v, want the fact cosine 0.50", b7.Document.Metadata[domain.MetaFactMatchScore])
	}
	if _, ok := on["c-5"]; ok {
		t.Fatal("a fact of a document outside the top N must not be added by the scoped lane")
	}
	calls := reader.scopedCalls()
	if len(calls) != 1 || !reflect.DeepEqual(calls[0].SourceDocumentIDs, []string{"doc-A", "doc-B"}) || calls[0].TopK != 20 {
		t.Fatalf("scoped search = %+v, want the first 2 DISTINCT documents in rank order, top 20", calls)
	}
	if calls[0].Scope == nil {
		t.Fatal("the scoped search must carry the caller's predicate")
	}

	// N=3 reaches doc-C, and then its fact comes in too.
	if _, ok := run(3)["c-5"]; !ok {
		t.Fatal("with N=3 doc-C is in scope and its fact's chunk must be added")
	}
}

func keys(m map[string]domain.SearchResult) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Scope: a scoped fact the caller's predicate refuses is never read, and a readable fact
// whose source chunk is out of scope yields nothing (the by-id fetch is enforced). With
// no predicate the enforcing reader refuses the scoped search too (fail closed).
func TestFactLaneScoped_NeverReturnsWhatTheCallerCannotRead(t *testing.T) {
	t.Parallel()
	chunks := []domain.Document{
		chunkOf("a-0", "doc-A", "public"), chunkOf("a-3", "doc-A", "public"),
		chunkOf("a-9", "doc-A", "secrets"),
	}
	store := &rankedChunkStore{laneChunkStore: &laneChunkStore{docs: chunks}, dense: []string{"a-0"}}
	reader := &scoredFactReader{facts: []domain.FactHit{
		// The fact itself is classified secret: the reader must never return it.
		scoredFact("secret-fact", "a-3", "doc-A", "Atlantis", 0.6, "secrets"),
		// A public fact keyed into a secret chunk (a mis-classified extraction).
		scoredFact("leaky-fact", "a-9", "doc-A", "El Dorado", 0.7, "public"),
	}}
	q := scopedLaneService(t, store, reader, &domain.TagPredicate{ForbiddenTags: []string{"secrets"}}, 3)
	res, err := q.Search(context.Background(), "where is it", "agent")
	if err != nil {
		t.Fatal(err)
	}
	got := collect(res)
	if got["a-9"] || got["a-3"] {
		t.Fatalf("an out-of-scope fact or chunk was returned through the scoped lane: %v", got)
	}
	for _, r := range res {
		for _, line := range factLines(r) {
			if strings.Contains(line, "Atlantis") || strings.Contains(line, "El Dorado") {
				t.Fatalf("an out-of-scope fact leaked as an annotation on %s: %q", r.Document.ID, line)
			}
		}
	}
	if calls := reader.scopedCalls(); len(calls) != 1 || calls[0].Scope == nil || len(calls[0].Scope.ForbiddenTags) != 1 {
		t.Fatalf("the scoped search must run under the caller's predicate: %+v", calls)
	}

	r := authz.NewEnforcingFactItemReader(reader, quietLog())
	if _, err := r.SearchFacts(context.Background(), []float32{1}, domain.FactSearchOptions{SourceDocumentIDs: []string{"doc-A"}}); !errors.Is(err, authz.ErrScopeMissing) {
		t.Fatalf("unscoped scoped-lane read: %v, want ErrScopeMissing", err)
	}
}

func TestScopedFactDocuments_FirstNDistinctInRankOrder(t *testing.T) {
	t.Parallel()
	rows := []domain.SearchResult{
		{Document: chunkOf("1", "d2")}, {Document: chunkOf("2", "d2")}, {Document: domain.Document{ID: "ep"}},
		{Document: chunkOf("3", "d1")}, {Document: chunkOf("4", "d3")}, {Document: chunkOf("5", "d4")},
	}
	if got := scopedFactDocuments(rows, 3); !reflect.DeepEqual(got, []string{"d2", "d1", "d3"}) {
		t.Fatalf("got %v", got)
	}
	if got := scopedFactDocuments(rows, 0); got != nil {
		t.Fatalf("N=0 must select nothing: %v", got)
	}
	if got := scopedFactDocuments(rows, 10); len(got) != 4 {
		t.Fatalf("fewer documents than N: %v", got)
	}
}
