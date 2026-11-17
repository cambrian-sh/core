package forget_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/memory/forget"
)

// chunkEraser resolves chunk ids the way postgres.PgErasureStore does since ENG-1947:
// a chunk of a stored document resolves to that DOCUMENT (Via = the chunk id); a
// document-less chunk is its own candidate. Document ids resolve to themselves.
type chunkEraser struct {
	rawEraser
	chunks map[string]domain.ErasureCandidate // chunk id → what it resolves to (Via filled in)
	docs   map[string]domain.ErasureCandidate
}

func (f *chunkEraser) Resolve(_ context.Context, sel domain.ErasureSelector) ([]domain.ErasureCandidate, error) {
	var out []domain.ErasureCandidate
	for _, id := range sel.DocumentIDs {
		if d, ok := f.docs[id]; ok {
			d.Via = id
			out = append(out, d)
		}
	}
	for _, id := range sel.ChunkIDs {
		if c, ok := f.chunks[id]; ok {
			c.Via = id
			out = append(out, c)
		}
	}
	return out, nil
}

// searcher records how it was called and returns fixed hits.
type searcher struct {
	hits     []domain.SearchResult
	err      error
	query    string
	callerID string
	scope    *domain.TagPredicate
	calls    int
}

func (s *searcher) Search(ctx context.Context, query, callerID string) ([]domain.SearchResult, error) {
	s.calls++
	s.query, s.callerID = query, callerID
	s.scope, _ = domain.ScopeFromContext(ctx)
	return s.hits, s.err
}

func hit(id, text string) domain.SearchResult {
	return domain.SearchResult{Document: domain.Document{ID: id, Text: text}}
}

func docCand(id, writer string) domain.ErasureCandidate {
	return domain.ErasureCandidate{Kind: domain.ErasureTargetDocument, ID: id, DocumentID: id, WrittenBy: writer}
}

// fixture: alice wrote doc A (chunks a1, a2) and a document-less chunk c1; bob wrote doc
// B (chunk b1), which alice can read; h1 is a hit hidden from alice by her predicate.
func queryFixture() *chunkEraser {
	a, b := docCand("docA", "alice"), docCand("docB", "bob")
	hidden := docCand("docH", "alice")
	hidden.Tags = []string{"secret"}
	return &chunkEraser{
		chunks: map[string]domain.ErasureCandidate{
			"a1": a, "a2": a, "b1": b, "h1": hidden,
			"c1": {Kind: domain.ErasureTargetChunk, ID: "c1", WrittenBy: "alice"},
		},
		docs: map[string]domain.ErasureCandidate{"docA": a, "docB": b, "docH": hidden},
	}
}

func newQueryService(raw *chunkEraser, srch forget.Searcher, snippet int) *forget.Service {
	e := authz.NewEnforcingEraser(raw, domain.AllowAllAuthorizer{}, authz.EraserPolicy{AgentErase: true}, nil)
	return forget.New(e, forget.Config{PreviewTTL: time.Minute, PolicyFingerprint: "fp", Searcher: srch, SnippetChars: snippet})
}

// aliceScoped is alice with a predicate that hides rows tagged "secret".
func aliceScoped() context.Context {
	return domain.WithScope(domain.WithPrincipal(context.Background(), domain.AgentPrincipal("alice")),
		&domain.TagPredicate{ForbiddenTags: []string{"secret"}})
}

// ENG-1948: the query is resolved by the caller's own search (same ctx predicate, the
// caller's id); hits widen to whole documents, are deduplicated in rank order, other
// writers' and hidden records are omitted, a document-less chunk stays a chunk, and
// every candidate carries a clipped snippet of its best-ranked hit.
func TestPreviewQuery_WholeDocumentsOwnOnlyWithSnippets(t *testing.T) {
	raw := queryFixture()
	srch := &searcher{hits: []domain.SearchResult{
		hit("b1", "bob's note about the salary"),
		hit("a2", "my   salary is\n 100k a year, paid monthly"),
		hit("h1", "hidden salary text"),
		hit("a1", "another passage of doc A"),
		hit("c1", "salary chunk"),
		hit("zz", "a substrate citation that is no chunk"),
	}}
	s := newQueryService(raw, srch, 12)
	ctx := aliceScoped()

	p, err := s.PreviewQuery(ctx, "  what I said about my salary ", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if srch.calls != 1 || srch.query != "what I said about my salary" || srch.callerID != "alice" ||
		srch.scope == nil || !slices.Equal(srch.scope.ForbiddenTags, []string{"secret"}) {
		t.Fatalf("search ran as %+v, want alice's own scoped search", srch)
	}
	var got []string
	for _, c := range p.Candidates {
		got = append(got, c.Kind+":"+c.ID+"<"+c.Via)
	}
	want := []string{"document:docA<a2", "chunk:c1<c1"}
	if !slices.Equal(got, want) {
		t.Fatalf("candidates = %v, want %v (bob's docB and hidden docH omitted, docA once)", got, want)
	}
	if !slices.Equal(p.Selector.DocumentIDs, []string{"docA"}) || !slices.Equal(p.Selector.ChunkIDs, []string{"c1"}) {
		t.Fatalf("selector = %+v", p.Selector)
	}
	if sn := p.Snippets[forget.SnippetKey(p.Candidates[0])]; sn != "my salary i…" {
		t.Fatalf("snippet = %q, want whitespace collapsed and clipped to 12 chars", sn)
	}
	if sn := p.Snippets[forget.SnippetKey(p.Candidates[1])]; sn != "salary chunk" {
		t.Fatalf("snippet = %q", sn)
	}
	for _, sn := range p.Snippets {
		if strings.Contains(sn, "bob") || strings.Contains(sn, "hidden") {
			t.Fatalf("a snippet quotes text alice did not write or cannot see: %q", sn)
		}
	}

	// The token is over the concrete ids: Forget erases exactly docA + c1.
	rec, err := s.Forget(ctx, forget.Request{PreviewToken: p.Token})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.erased) != 1 || !slices.Equal(raw.erased[0].Selector.DocumentIDs, []string{"docA"}) ||
		!slices.Equal(raw.erased[0].Selector.ChunkIDs, []string{"c1"}) || rec.Principal != "alice" {
		t.Fatalf("erased = %+v", raw.erased)
	}
	// The same query again is the same preview token (deterministic over the ids).
	again, err := s.PreviewQuery(ctx, "what I said about my salary", "alice")
	if err == nil && again.Token != p.Token {
		t.Fatalf("token not deterministic: %q vs %q", again.Token, p.Token)
	}
}

// Only other writers' hits → nothing to erase (NotFound at the RPC), never a denial
// that would confirm the records exist.
func TestPreviewQuery_OnlyOthersHitsIsNothingToErase(t *testing.T) {
	srch := &searcher{hits: []domain.SearchResult{hit("b1", "bob's text")}}
	s := newQueryService(queryFixture(), srch, 0)
	if _, err := s.PreviewQuery(aliceScoped(), "salary", "alice"); !errors.Is(err, domain.ErrNothingToErase) {
		t.Fatalf("err = %v, want ErrNothingToErase", err)
	}
	srch.hits = nil
	if _, err := s.PreviewQuery(aliceScoped(), "salary", "alice"); !errors.Is(err, domain.ErrNothingToErase) {
		t.Fatalf("no hits: err = %v, want ErrNothingToErase", err)
	}
}

// Refusals: disabled erasure, an empty query, no searcher, no principal, a failing
// search — each distinct, and none reaches the search when it need not.
func TestPreviewQuery_Refusals(t *testing.T) {
	srch := &searcher{}
	if _, err := forget.New(nil, forget.Config{Searcher: srch}).PreviewQuery(aliceScoped(), "x", "alice"); !errors.Is(err, domain.ErrErasureDisabled) {
		t.Fatalf("disabled: %v", err)
	}
	s := newQueryService(queryFixture(), srch, 0)
	if _, err := s.PreviewQuery(aliceScoped(), "  ", "alice"); !errors.Is(err, domain.ErrInvalidErasureRequest) {
		t.Fatalf("empty query: %v", err)
	}
	if _, err := newQueryService(queryFixture(), nil, 0).PreviewQuery(aliceScoped(), "x", "alice"); !errors.Is(err, forget.ErrQueryUnavailable) {
		t.Fatalf("no searcher: %v", err)
	}
	if _, err := s.PreviewQuery(context.Background(), "x", "alice"); !errors.Is(err, forget.ErrNoPrincipal) {
		t.Fatalf("no principal: %v", err)
	}
	if srch.calls != 0 {
		t.Fatalf("search ran %d times on refused requests", srch.calls)
	}
	srch.err = errors.New("boom")
	if _, err := s.PreviewQuery(aliceScoped(), "x", "alice"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("search failure: %v", err)
	}
}

// forget_memory with `query`: previews whole documents with snippets, refuses query +
// ids and query + token, and the token erases exactly the previewed set.
func TestForgetTool_QueryArgument(t *testing.T) {
	raw := queryFixture()
	srch := &searcher{hits: []domain.SearchResult{hit("a1", "doc A text"), hit("b1", "bob")}}
	tool := forget.NewTool(newQueryService(raw, srch, 0), true)
	ctx := domain.WithScope(context.Background(), &domain.TagPredicate{})
	if !strings.Contains(tool.Spec().Description, "WHOLE DOCUMENT") || !strings.Contains(string(tool.Spec().Schema), `"query"`) {
		t.Fatal("forget_memory must document the query argument and whole-document erasure")
	}
	out, err := tool.Call(ctx, domain.KernelToolCall{AgentID: "alice", ArgsJSON: []byte(`{"query":"doc A"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var prev struct {
		Erased       bool   `json:"erased"`
		PreviewToken string `json:"preview_token"`
		WouldErase   []struct {
			Kind, ID, Snippet string
		} `json:"would_erase"`
	}
	if err := json.Unmarshal(out, &prev); err != nil || prev.Erased || len(prev.WouldErase) != 1 ||
		prev.WouldErase[0].ID != "docA" || prev.WouldErase[0].Snippet != "doc A text" || srch.callerID != "alice" {
		t.Fatalf("preview = %s (%v)", out, err)
	}
	for _, bad := range []string{`{"query":"x","document_ids":["docA"]}`, `{"query":"x","preview_token":"` + prev.PreviewToken + `"}`} {
		if _, err := tool.Call(ctx, domain.KernelToolCall{AgentID: "alice", ArgsJSON: []byte(bad)}); err == nil {
			t.Fatalf("%s must be refused", bad)
		}
	}
	if len(raw.erased) != 0 {
		t.Fatal("a preview erased something")
	}
	if _, err := tool.Call(ctx, domain.KernelToolCall{AgentID: "alice", ArgsJSON: []byte(`{"preview_token":"` + prev.PreviewToken + `"}`)}); err != nil {
		t.Fatal(err)
	}
	if len(raw.erased) != 1 || !slices.Equal(raw.erased[0].Selector.DocumentIDs, []string{"docA"}) {
		t.Fatalf("erased = %+v", raw.erased)
	}
}
