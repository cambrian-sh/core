package domain

import (
	"context"
	"strings"
	"testing"
)

func toolRegFixture() *InMemoryToolRegistry {
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "web_search", Description: "search the web and return results",
		Schema: []byte(`{"properties":{"query":{"type":"string"},"max_results":{"type":"integer"}}}`)})
	reg.Register(SystemTool{Name: "read_file", Description: "read a text file from disk",
		Schema: []byte(`{"properties":{"path":{"type":"string"}}}`)})
	reg.Register(SystemTool{Name: "execute_command", Description: "run a shell command"})
	return reg
}

// BuildToolDoc carries name + description + arg names, document-prefixed (D5).
func TestBuildToolDoc(t *testing.T) {
	doc := BuildToolDoc(SystemTool{
		Name: "web_search", Description: "search the web",
		Schema: []byte(`{"properties":{"query":{"type":"string"},"max_results":{"type":"integer"}}}`),
	})
	if !strings.HasPrefix(doc, "search_document: ") {
		t.Errorf("doc must carry the asymmetric document prefix, got %q", doc)
	}
	for _, want := range []string{"web_search", "search the web", "query", "max_results"} {
		if !strings.Contains(doc, want) {
			t.Errorf("doc should contain %q, got %q", want, doc)
		}
	}
}

// The keyword retriever ranks granted tools by query overlap and returns top-k.
func TestKeywordToolRetriever_RanksAndCaps(t *testing.T) {
	r := KeywordToolRetriever{Registry: toolRegFixture()}
	got, err := r.Rank(context.Background(), "search the web for a person", []string{"web_search", "read_file", "execute_command"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "web_search" {
		t.Errorf("top-1 for a web-search need should be web_search, got %v", got)
	}
}

// AvailableToolsRanked grant-filters first, then ranks within the authorized set;
// no query / fits-in-k ⇒ full menu; an empty ranked result is honored.
func TestAvailableToolsRanked(t *testing.T) {
	reg := toolRegFixture()
	exec := &ToolExecutor{
		Registry:     reg,
		Grants:       NewInMemoryGrantsStore(),
		Unrestricted: true, // all 3 tools authorized
		Retriever:    KeywordToolRetriever{Registry: reg},
	}
	ctx := context.Background()

	// Ranked: a web-search task with k=1 narrows to web_search.
	got := exec.AvailableToolsRanked(ctx, "a", "search the web", 1)
	if len(got) != 1 || got[0].Name != "web_search" {
		t.Errorf("ranked menu should be [web_search], got %v", toolNames(got))
	}

	// No query ⇒ full menu (backward compatible).
	if got := exec.AvailableToolsRanked(ctx, "a", "", 1); len(got) != 3 {
		t.Errorf("empty query should return the full menu (3), got %d", len(got))
	}

	// k >= set size ⇒ full menu (nothing to narrow).
	if got := exec.AvailableToolsRanked(ctx, "a", "search", 10); len(got) != 3 {
		t.Errorf("k>=size should return the full menu, got %d", len(got))
	}

	// An anonymous principal still gets nothing (authorization preserved).
	if got := exec.AvailableToolsRanked(ctx, "", "search", 1); got != nil {
		t.Errorf("anonymous principal must get no tools, got %v", toolNames(got))
	}
}

// AvailableToolsNamed (describe_tool, ADR-0045 D6) returns a named tool only
// when it is granted — an ungranted/unknown name is absent (fail-closed, no
// existence leak); an anonymous principal gets nothing.
func TestAvailableToolsNamed(t *testing.T) {
	reg := toolRegFixture()
	grants := NewInMemoryGrantsStore()
	grants.Set("agent1", []ToolGrant{{Tool: "web_search"}})
	exec := &ToolExecutor{Registry: reg, Grants: grants}
	ctx := context.Background()

	if got := exec.AvailableToolsNamed(ctx, "agent1", []string{"web_search"}); len(got) != 1 || got[0].Name != "web_search" {
		t.Errorf("granted name should return the tool, got %v", toolNames(got))
	}
	if got := exec.AvailableToolsNamed(ctx, "agent1", []string{"execute_command"}); len(got) != 0 {
		t.Errorf("existing-but-ungranted name must be absent, got %v", toolNames(got))
	}
	if got := exec.AvailableToolsNamed(ctx, "agent1", []string{"nope"}); len(got) != 0 {
		t.Errorf("unknown name must be absent, got %v", toolNames(got))
	}
	if got := exec.AvailableToolsNamed(ctx, "", []string{"web_search"}); got != nil {
		t.Errorf("anonymous principal must get nothing, got %v", toolNames(got))
	}
}

// --- VectorToolRetriever (0044-03) ---

type fakeEmbedder struct{ lastText string }

func (f *fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	f.lastText = text
	return []float32{0.1, 0.2, 0.3}, nil
}

type fakeToolStore struct {
	lastFilter  string
	lastDocType string
	lastScope   *EffectiveScope
	results     []SearchResult
	saved       map[string]*Document // by ID — tracks upserts
	deleted     []string
}

func (s *fakeToolStore) Search(_ context.Context, _ []float32, opts SearchOptions) ([]SearchResult, error) {
	s.lastFilter = opts.Filter
	s.lastDocType = opts.DocumentType
	s.lastScope = opts.Scope
	return s.results, nil
}
func (s *fakeToolStore) Save(_ context.Context, d *Document) error {
	if s.saved == nil {
		s.saved = map[string]*Document{}
	}
	s.saved[d.ID] = d // upsert by ID
	return nil
}
func (s *fakeToolStore) SaveBatch(context.Context, []*Document) error          { return nil }
func (s *fakeToolStore) GetByID(context.Context, string) (*Document, error)    { return nil, nil }
func (s *fakeToolStore) GetBatch(context.Context, []string) ([]Document, error) { return nil, nil }
func (s *fakeToolStore) Delete(_ context.Context, id string) error {
	s.deleted = append(s.deleted, id)
	if s.saved != nil {
		delete(s.saved, id)
	}
	return nil
}
func (s *fakeToolStore) DeleteBatch(context.Context, []string) error           { return nil }
func (s *fakeToolStore) IncrementAccess(context.Context, string) error         { return nil }
func (s *fakeToolStore) GetStaleMemories(context.Context, int) ([]Document, error) {
	return nil, nil
}
func (s *fakeToolStore) QueryByMetadata(context.Context, map[string]string, int) ([]Document, error) {
	return nil, nil
}

// The vector retriever applies the search_query prefix, the grant filter, and the
// relevance floor (low-score candidates dropped).
func TestVectorToolRetriever_FloorAndGrantFilter(t *testing.T) {
	emb := &fakeEmbedder{}
	store := &fakeToolStore{results: []SearchResult{
		{Document: Document{ID: "web_search"}, RawScore: 0.70},
		{Document: Document{ID: "read_file"}, RawScore: 0.20}, // below floor
	}}
	r := VectorToolRetriever{Store: store, Embedder: emb, Floor: 0.30}

	got, err := r.Rank(context.Background(), "search the web", []string{"web_search", "read_file"}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "web_search" {
		t.Errorf("floor should drop the 0.20 result, got %v", got)
	}
	if !strings.HasPrefix(emb.lastText, "search_query: ") {
		t.Errorf("query must carry the asymmetric query prefix, got %q", emb.lastText)
	}
	if !strings.Contains(store.lastFilter, "id IN") || !strings.Contains(store.lastFilter, "web_search") {
		t.Errorf("grant filter should restrict to granted ids, got %q", store.lastFilter)
	}
}

// Nothing above the floor ⇒ an empty menu (the grounding safeguard).
func TestVectorToolRetriever_EmptyWhenNothingClearsFloor(t *testing.T) {
	emb := &fakeEmbedder{}
	store := &fakeToolStore{results: []SearchResult{{Document: Document{ID: "read_file"}, RawScore: 0.10}}}
	r := VectorToolRetriever{Store: store, Embedder: emb, Floor: 0.50}

	got, _ := r.Rank(context.Background(), "search the web", []string{"read_file"}, 3)
	if len(got) != 0 {
		t.Errorf("nothing clears the floor ⇒ empty menu, got %v", got)
	}
}

// No granted names (tools_unrestricted) ⇒ no SQL filter (rank over all tools).
func TestVectorToolRetriever_UnrestrictedNoFilter(t *testing.T) {
	emb := &fakeEmbedder{}
	store := &fakeToolStore{results: []SearchResult{{Document: Document{ID: "web_search"}, RawScore: 0.9}}}
	r := VectorToolRetriever{Store: store, Embedder: emb}

	if _, err := r.Rank(context.Background(), "x", nil, 3); err != nil {
		t.Fatal(err)
	}
	if store.lastFilter != "" {
		t.Errorf("unrestricted (no granted names) should set no filter, got %q", store.lastFilter)
	}
}

// --- ToolIndexer (0044-04) ---

// Indexing embeds the tool doc and upserts it as a DocTypeTool keyed by name;
// re-indexing the same tool upserts (no duplicate). Remove drops it.
func TestToolIndexer_IndexUpsertAndRemove(t *testing.T) {
	store := &fakeToolStore{}
	ix := &ToolIndexer{Store: store, Embedder: &fakeEmbedder{}}
	tool := SystemTool{Name: "web_search", Description: "search the web",
		Schema: []byte(`{"properties":{"query":{"type":"string"}}}`)}

	if err := ix.Index(context.Background(), tool); err != nil {
		t.Fatal(err)
	}
	if err := ix.Index(context.Background(), tool); err != nil { // re-index
		t.Fatal(err)
	}
	if len(store.saved) != 1 {
		t.Errorf("re-indexing the same tool must upsert (1 doc), got %d", len(store.saved))
	}
	doc := store.saved["web_search"]
	if doc == nil || doc.DocumentType != DocTypeTool {
		t.Fatalf("tool indexed under wrong type: %+v", doc)
	}
	if !strings.Contains(doc.Text, "web_search") || len(doc.Embedding.Vector) == 0 {
		t.Errorf("indexed doc should carry the built text + embedding, got %+v", doc)
	}

	if err := ix.Remove(context.Background(), "web_search"); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.saved["web_search"]; ok {
		t.Error("Remove should drop the tool doc")
	}
}
