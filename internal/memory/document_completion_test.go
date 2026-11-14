package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// ENG-430 (owner decision 2026-11-14, third round): document/session completion.

// chainStore is a raw store holding chained chunks. Search returns only the ids in
// `ranked` (as the ranking would); GetByID serves every chunk unfiltered, like the
// raw adapter — enforcement is the decorator's job.
type chainStore struct {
	byID   map[string]domain.Document
	ranked []string
}

func (s *chainStore) Search(context.Context, []float32, domain.SearchOptions) ([]domain.SearchResult, error) {
	var out []domain.SearchResult
	for i, id := range s.ranked {
		sc := 0.9 - float64(i)*0.01
		out = append(out, domain.SearchResult{Document: s.byID[id], Score: sc, RawScore: sc})
	}
	return out, nil
}
func (s *chainStore) GetByID(_ context.Context, id string) (*domain.Document, error) {
	d, ok := s.byID[id]
	if !ok {
		return nil, nil
	}
	return &d, nil
}
func (s *chainStore) GetBatch(ctx context.Context, ids []string) ([]domain.Document, error) {
	var out []domain.Document
	for _, id := range ids {
		if d, ok := s.byID[id]; ok {
			out = append(out, d)
		}
	}
	return out, nil
}
func (s *chainStore) Save(context.Context, *domain.Document) error        { return nil }
func (s *chainStore) SaveBatch(context.Context, []*domain.Document) error { return nil }
func (s *chainStore) Delete(context.Context, string) error                { return nil }
func (s *chainStore) DeleteBatch(context.Context, []string) error         { return nil }
func (s *chainStore) IncrementAccess(context.Context, string) error       { return nil }
func (s *chainStore) GetStaleMemories(context.Context, int) ([]domain.Document, error) {
	return nil, nil
}
func (s *chainStore) QueryByMetadata(context.Context, map[string]string, int) ([]domain.Document, error) {
	return nil, nil
}

// addDoc chains n chunks "<doc>-0".."<doc>-(n-1)" of one document. tagsFor may give a
// chunk tags (nil ⇒ untagged).
func (s *chainStore) addDoc(doc string, n int, tagsFor func(i int) []string) {
	if s.byID == nil {
		s.byID = map[string]domain.Document{}
	}
	id := func(i int) string { return fmt.Sprintf("%s-%d", doc, i) }
	for i := 0; i < n; i++ {
		cr := ChunkRelations{}
		if i > 0 {
			cr.PrecedingChunkID = id(i - 1)
		}
		if i < n-1 {
			cr.FollowingChunkID = id(i + 1)
		}
		raw, _ := json.Marshal(cr)
		meta := map[string]interface{}{"document_id": doc, "chunk_relations": string(raw)}
		if tagsFor != nil {
			if tags := tagsFor(i); tags != nil {
				meta["tags"] = tags
			}
		}
		s.byID[id(i)] = domain.Document{ID: id(i), Text: "item " + id(i), DocumentType: domain.DocTypeMnemonicFact, Metadata: meta}
	}
}

func completionIDs(rs []domain.SearchResult) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Document.ID
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func rankedRows(s *chainStore, idList ...string) []domain.SearchResult {
	out := make([]domain.SearchResult, 0, len(idList))
	for i, id := range idList {
		out = append(out, domain.SearchResult{Document: s.byID[id], Score: 0.9 - float64(i)*0.01})
	}
	return out
}

func TestDocumentCompletion_OffLeavesResultsUnchanged(t *testing.T) {
	t.Parallel()
	s := &chainStore{}
	s.addDoc("A", 5, nil)
	q := NewQueryService(&fakeEmbedder{}, s, nil)
	in := rankedRows(s, "A-2")
	if got := completionIDs(q.applyDocumentCompletion(context.Background(), in)); !eq(got, []string{"A-2"}) {
		t.Fatalf("off: %v", got)
	}
}

// The whole short document comes back after every ranked row, in document order,
// with no duplicate of a chunk that was already ranked; only the top document is
// completed by default.
func TestDocumentCompletion_ReturnsTheRestOfTheTopDocumentInOrder(t *testing.T) {
	t.Parallel()
	s := &chainStore{}
	s.addDoc("A", 6, nil)
	s.addDoc("B", 4, nil)
	q := NewQueryService(&fakeEmbedder{}, s, nil)
	q.EnableDocumentCompletion(0, 0) // defaults: 1 document, 24 chunks
	in := rankedRows(s, "A-3", "B-1", "A-5")
	got := completionIDs(q.applyDocumentCompletion(context.Background(), in))
	want := []string{"A-3", "B-1", "A-5", "A-0", "A-1", "A-2", "A-4"}
	if !eq(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// The cap splits the budget around the hit (alternating sides), and documents=2
// completes the second document too.
func TestDocumentCompletion_CapAndDocumentCount(t *testing.T) {
	t.Parallel()
	s := &chainStore{}
	s.addDoc("A", 10, nil)
	s.addDoc("B", 3, nil)
	q := NewQueryService(&fakeEmbedder{}, s, nil)
	q.EnableDocumentCompletion(2, 3)
	got := completionIDs(q.applyDocumentCompletion(context.Background(), rankedRows(s, "A-5", "B-0")))
	want := []string{"A-5", "B-0", "A-3", "A-4", "A-6", "B-1", "B-2"}
	if !eq(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// End to end through Search over the REAL enforcing read store: a chunk the caller
// may not read stops the walk on its side and never appears; the other side is
// completed. Scope travels on ctx exactly as in production.
func TestDocumentCompletion_SearchThroughEnforcingStoreHidesForbiddenChunks(t *testing.T) {
	t.Parallel()
	raw := &chainStore{ranked: []string{"S-2"}}
	raw.addDoc("S", 6, func(i int) []string {
		if i == 4 {
			return []string{"secrets"}
		}
		return nil
	})
	read := authz.NewEnforcingVectorStore(raw, slog.Default())
	q := NewQueryService(&fakeEmbedder{}, read, &policyAuthorizer{
		known: map[string]bool{"support": true},
		preds: map[string]*domain.TagPredicate{"support": {ForbiddenTags: []string{"secrets"}}},
	})
	q.EnableDocumentCompletion(1, 24)
	res, err := q.Search(context.Background(), "the 3rd item", "support")
	if err != nil {
		t.Fatal(err)
	}
	got := collect(res)
	for _, id := range []string{"S-0", "S-1", "S-2", "S-3"} {
		if !got[id] {
			t.Errorf("completion missed %s: %v", id, completionIDs(res))
		}
	}
	if got["S-4"] || got["S-5"] {
		t.Fatalf("a forbidden chunk (or one only reachable through it) was returned: %v", completionIDs(res))
	}
	if completionIDs(res)[0] != "S-2" {
		t.Fatalf("the ranked hit must stay first: %v", completionIDs(res))
	}
}

// A chain that points into another document is not followed.
func TestDocumentCompletion_DoesNotCrossDocuments(t *testing.T) {
	t.Parallel()
	s := &chainStore{}
	s.addDoc("A", 2, nil)
	s.addDoc("B", 2, nil)
	d := s.byID["A-1"]
	raw, _ := json.Marshal(ChunkRelations{PrecedingChunkID: "A-0", FollowingChunkID: "B-0"})
	d.Metadata["chunk_relations"] = string(raw)
	s.byID["A-1"] = d
	q := NewQueryService(&fakeEmbedder{}, s, nil)
	q.EnableDocumentCompletion(1, 24)
	got := completionIDs(q.applyDocumentCompletion(context.Background(), rankedRows(s, "A-0")))
	if !eq(got, []string{"A-0", "A-1"}) {
		t.Fatalf("got %v", got)
	}
}
