package memory

import (
	"context"
	"reflect"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

func TestExtractQueryTerms(t *testing.T) {
	got := extractQueryTerms("When did Caroline go to the LGBTQ support group?")
	// Content unigrams + adjacent bigrams + adjacent TRIGRAMS (2026-08-14: the
	// bigram cap meant 3-word entity anchors like "stadio ciro vigorito" could
	// never match the graph lane; affordable since the lookup became one batched
	// query — review Q8). Stopwords (when/did/the/go/to) and <3-char tokens
	// dropped; adjacency is over the remaining content words.
	want := []string{
		"caroline", "caroline lgbtq", "caroline lgbtq support",
		"lgbtq", "lgbtq support", "lgbtq support group",
		"support", "support group", "group",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("extractQueryTerms mismatch:\n got=%v\nwant=%v", got, want)
	}
}

// fakeVecStore is a minimal VectorStore: only GetByID is exercised by query-entity
// seeding (it materializes entity-matched chunk IDs into SearchResults).
type fakeVecStore struct{ docs map[string]domain.Document }

func (f *fakeVecStore) GetByID(_ context.Context, id string) (*domain.Document, error) {
	if d, ok := f.docs[id]; ok {
		return &d, nil
	}
	return nil, nil
}
func (f *fakeVecStore) Save(context.Context, *domain.Document) error        { return nil }
func (f *fakeVecStore) SaveBatch(context.Context, []*domain.Document) error { return nil }
func (f *fakeVecStore) Search(context.Context, []float32, domain.SearchOptions) ([]domain.SearchResult, error) {
	return nil, nil
}
func (f *fakeVecStore) GetBatch(context.Context, []string) ([]domain.Document, error) {
	return nil, nil
}
func (f *fakeVecStore) Delete(context.Context, string) error          { return nil }
func (f *fakeVecStore) DeleteBatch(context.Context, []string) error   { return nil }
func (f *fakeVecStore) IncrementAccess(context.Context, string) error { return nil }
func (f *fakeVecStore) GetStaleMemories(context.Context, int) ([]domain.Document, error) {
	return nil, nil
}
func (f *fakeVecStore) QueryByMetadata(context.Context, map[string]string, int) ([]domain.Document, error) {
	return nil, nil
}

// TestInjectQueryEntitySeeds_RescuesVectorMiss: the gold chunk is NOT in the
// vector pool, but it mentions an entity present in the query ("caroline"). Query-
// entity seeding must pull it into the pool via ChunksMentioningEntity.
func TestInjectQueryEntitySeeds_RescuesVectorMiss(t *testing.T) {
	st := newFakeChunkTripletsStore()
	// gold-1 mentions caroline; an unrelated chunk does not.
	_ = st.SaveChunkTriplets(context.Background(), "gold-1", []domain.ChunkTriplet{
		{H: "caroline", R: "moved from", T: "seattle"},
	})
	_ = st.SaveChunkTriplets(context.Background(), "noise", []domain.ChunkTriplet{
		{H: "tim", R: "read", T: "book"},
	})
	vs := &fakeVecStore{docs: map[string]domain.Document{
		"gold-1": {ID: "gold-1", Text: "I moved here from Seattle"},
		"noise":  {ID: "noise", Text: "Tim read a book"},
	}}
	q := &QueryService{chunkTriplets: st, vectorStore: vs, queryEntitySeed: true, kgPerEntity: 5, kgMaxExpanded: 20}

	// Vector pool is EMPTY (total miss). Seeding must still surface gold-1.
	out := q.injectQueryEntitySeeds(context.Background(), nil, "Where did Caroline move from?", nil, &domain.TagPredicate{Bypass: true})

	var foundGold, foundNoise bool
	for _, r := range out {
		if r.Document.ID == "gold-1" {
			foundGold = true
		}
		if r.Document.ID == "noise" {
			foundNoise = true
		}
	}
	if !foundGold {
		t.Fatalf("query-entity seeding must rescue the gold chunk via 'caroline'; got %d results", len(out))
	}
	if foundNoise {
		t.Fatalf("a chunk sharing no query entity must NOT be seeded")
	}
}

// TestInjectQueryEntitySeeds_DedupAndDisabled: already-present chunks aren't
// duplicated, and an empty/entity-less query is a no-op.
func TestInjectQueryEntitySeeds_DedupAndNoop(t *testing.T) {
	st := newFakeChunkTripletsStore()
	_ = st.SaveChunkTriplets(context.Background(), "g", []domain.ChunkTriplet{{H: "melanie", R: "painted", T: "sunset"}})
	vs := &fakeVecStore{docs: map[string]domain.Document{"g": {ID: "g"}}}
	q := &QueryService{chunkTriplets: st, vectorStore: vs, queryEntitySeed: true, kgPerEntity: 5, kgMaxExpanded: 20}

	// "g" already in the pool ⇒ no duplicate.
	pool := []domain.SearchResult{{Document: domain.Document{ID: "g"}, RawScore: 0.9}}
	out := q.injectQueryEntitySeeds(context.Background(), pool, "What did Melanie paint?", nil, &domain.TagPredicate{Bypass: true})
	if len(out) != 1 {
		t.Fatalf("existing chunk must not be duplicated, got %d", len(out))
	}
	// All-stopword query ⇒ no terms ⇒ unchanged pool.
	out2 := q.injectQueryEntitySeeds(context.Background(), pool, "what did they do?", nil, &domain.TagPredicate{Bypass: true})
	if len(out2) != 1 {
		t.Fatalf("entity-less query must be a no-op, got %d", len(out2))
	}
}
