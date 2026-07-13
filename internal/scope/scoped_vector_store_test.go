package scope_test

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/scope"
)

// fakeStore is a minimal VectorStore that applies the effective scope predicate
// to a seeded corpus via domain.EffectiveScope.Allows — mirroring what the
// pgvector adapter does in SQL, so the decorator can be tested without Postgres.
type fakeStore struct {
	docs       []domain.Document
	lastOpts   domain.SearchOptions
	searchCald bool
}

func (f *fakeStore) Search(_ context.Context, _ []float32, opts domain.SearchOptions) ([]domain.SearchResult, error) {
	f.searchCald = true
	f.lastOpts = opts
	var out []domain.SearchResult
	for _, d := range f.docs {
		var tags []string
		if raw, ok := d.Metadata["tags"].([]string); ok {
			tags = raw
		}
		if opts.Scope.Allows(tags) {
			out = append(out, domain.SearchResult{Document: d})
		}
	}
	return out, nil
}

// Unused interface methods.
func (f *fakeStore) Save(context.Context, *domain.Document) error              { return nil }
func (f *fakeStore) SaveBatch(context.Context, []*domain.Document) error       { return nil }
func (f *fakeStore) GetByID(context.Context, string) (*domain.Document, error) { return nil, nil }
func (f *fakeStore) GetBatch(context.Context, []string) ([]domain.Document, error) {
	return nil, nil
}
func (f *fakeStore) Delete(context.Context, string) error        { return nil }
func (f *fakeStore) DeleteBatch(context.Context, []string) error { return nil }
func (f *fakeStore) IncrementAccess(context.Context, string) error {
	return nil
}
func (f *fakeStore) GetStaleMemories(context.Context, int) ([]domain.Document, error) {
	return nil, nil
}
func (f *fakeStore) QueryByMetadata(context.Context, map[string]string, int) ([]domain.Document, error) {
	return nil, nil
}

func doc(id string, tags ...string) domain.Document {
	return domain.Document{ID: id, Metadata: map[string]interface{}{"tags": tags}}
}

func seeded() *fakeStore {
	return &fakeStore{docs: []domain.Document{
		doc("public", "public_kb"),
		doc("secret", "secrets"),
		doc("order", "order_db", "public_kb"),
	}}
}

func ids(rs []domain.SearchResult) map[string]bool {
	m := map[string]bool{}
	for _, r := range rs {
		m[r.Document.ID] = true
	}
	return m
}

func TestScopedSearch_FailsClosedWithoutScope(t *testing.T) {
	store := seeded()
	sv := scope.NewScopedVectorStore(store, nil)

	_, err := sv.Search(context.Background(), nil, domain.SearchOptions{})
	if !errors.Is(err, scope.ErrScopeMissing) {
		t.Fatalf("expected ErrScopeMissing, got %v", err)
	}
	if store.searchCald {
		t.Fatalf("underlying store must NOT be queried when scope is missing")
	}
}

func TestScopedSearch_ForbiddenTagExcludesDocs(t *testing.T) {
	store := seeded()
	sv := scope.NewScopedVectorStore(store, nil)
	eff := domain.NewEffectiveScope(domain.ScopeConfig{ForbiddenTags: []string{"secrets"}}, domain.ScopeConfig{})

	res, err := sv.Search(context.Background(), nil, domain.SearchOptions{Scope: &eff})
	if err != nil {
		t.Fatal(err)
	}
	got := ids(res)
	if got["secret"] {
		t.Errorf("secrets-tagged doc must be excluded, got %v", got)
	}
	if !got["public"] || !got["order"] {
		t.Errorf("non-secret docs must be returned, got %v", got)
	}
}

func TestScopedSearch_ScopeFromContext(t *testing.T) {
	store := seeded()
	sv := scope.NewScopedVectorStore(store, nil)
	eff := domain.NewEffectiveScope(domain.ScopeConfig{RequiredTags: []string{"order_db"}}, domain.ScopeConfig{})

	ctx := domain.WithScope(context.Background(), &eff)
	res, err := sv.Search(ctx, nil, domain.SearchOptions{}) // no explicit opts.Scope
	if err != nil {
		t.Fatal(err)
	}
	got := ids(res)
	if len(got) != 1 || !got["order"] {
		t.Errorf("expected only order_db doc, got %v", got)
	}
}

func TestScopedSearch_ScopeSystemBypasses(t *testing.T) {
	store := seeded()
	sv := scope.NewScopedVectorStore(store, nil)

	res, err := sv.Search(context.Background(), nil, domain.SearchOptions{Scope: domain.ScopeSystem})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 3 {
		t.Errorf("ScopeSystem must return all docs, got %d", len(res))
	}
}

func TestScopedSearch_ExplicitScopeBeatsContext(t *testing.T) {
	store := seeded()
	sv := scope.NewScopedVectorStore(store, nil)
	ctxScope := domain.ScopeSystem
	explicit := domain.NewEffectiveScope(domain.ScopeConfig{ForbiddenTags: []string{"secrets"}}, domain.ScopeConfig{})

	ctx := domain.WithScope(context.Background(), ctxScope)
	res, err := sv.Search(ctx, nil, domain.SearchOptions{Scope: &explicit})
	if err != nil {
		t.Fatal(err)
	}
	if ids(res)["secret"] {
		t.Errorf("explicit opts.Scope must take precedence over ctx scope")
	}
}
