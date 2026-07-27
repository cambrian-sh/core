package authz_test

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// fakeStore is a minimal VectorStore that applies the predicate to a seeded
// corpus via domain.TagPredicate.Allows — mirroring what the pgvector adapter
// does in SQL, so the chokepoint can be tested without Postgres.
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

func TestEnforcedSearch_FailsClosedWithoutPredicate(t *testing.T) {
	store := seeded()
	sv := authz.NewEnforcingVectorStore(store, nil)

	_, err := sv.Search(context.Background(), nil, domain.SearchOptions{})
	if !errors.Is(err, authz.ErrScopeMissing) {
		t.Fatalf("expected ErrScopeMissing, got %v", err)
	}
	if store.searchCald {
		t.Fatalf("underlying store must NOT be queried when the predicate is missing")
	}
}

func TestEnforcedSearch_ForbiddenTagExcludesDocs(t *testing.T) {
	store := seeded()
	sv := authz.NewEnforcingVectorStore(store, nil)
	eff := &domain.TagPredicate{ForbiddenTags: []string{"secrets"}}

	res, err := sv.Search(context.Background(), nil, domain.SearchOptions{Scope: eff})
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

func TestEnforcedSearch_PredicateFromContext(t *testing.T) {
	store := seeded()
	sv := authz.NewEnforcingVectorStore(store, nil)
	eff := &domain.TagPredicate{RequiredTags: []string{"order_db"}}

	ctx := domain.WithScope(context.Background(), eff)
	res, err := sv.Search(ctx, nil, domain.SearchOptions{}) // no explicit opts.Scope
	if err != nil {
		t.Fatal(err)
	}
	got := ids(res)
	if len(got) != 1 || !got["order"] {
		t.Errorf("expected only order_db doc, got %v", got)
	}
}

func TestEnforcedSearch_ScopeSystemBypasses(t *testing.T) {
	store := seeded()
	sv := authz.NewEnforcingVectorStore(store, nil)

	res, err := sv.Search(context.Background(), nil, domain.SearchOptions{Scope: domain.ScopeSystem})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 3 {
		t.Errorf("the bypass sentinel must return all docs, got %d", len(res))
	}
}

func TestEnforcedSearch_ExplicitPredicateBeatsContext(t *testing.T) {
	store := seeded()
	sv := authz.NewEnforcingVectorStore(store, nil)
	explicit := &domain.TagPredicate{ForbiddenTags: []string{"secrets"}}

	ctx := domain.WithScope(context.Background(), domain.ScopeSystem)
	res, err := sv.Search(ctx, nil, domain.SearchOptions{Scope: explicit})
	if err != nil {
		t.Fatal(err)
	}
	if ids(res)["secret"] {
		t.Errorf("explicit opts.Scope must take precedence over the ctx predicate")
	}
}

// The OSS default must reach the store, not be refused: unrestricted IS the
// policy when no plugin is installed (§4.2). This is the OSS mirror of the
// plugin's fail-closed test.
func TestEnforcedSearch_AllowAllAuthorizerReachesTheStore(t *testing.T) {
	store := seeded()
	sv := authz.NewEnforcingVectorStore(store, nil)

	pred, dec := domain.AllowAllAuthorizer{}.ReadFilter(context.Background(), domain.AgentPrincipal("anyone"), domain.SurfaceRef{})
	if !dec.Allowed {
		t.Fatalf("OSS must not deny")
	}
	res, err := sv.Search(context.Background(), nil, domain.SearchOptions{Scope: pred})
	if err != nil {
		t.Fatalf("an unscoped OSS deployment must not fail closed: %v", err)
	}
	if len(res) != 3 {
		t.Errorf("OSS reads every row, got %d", len(res))
	}
}
