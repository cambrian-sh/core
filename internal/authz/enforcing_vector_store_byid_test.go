package authz_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// byIDStore serves documents by identity. Unlike the Search path there is no
// opts.Scope to push down, so this fake applies NO filtering of its own — the
// decorator must do it, which is exactly what these tests pin.
type byIDStore struct {
	docs     map[string]domain.Document
	getCalls int
}

func tagged(id string, tags ...string) domain.Document {
	d := domain.Document{ID: id, Text: "secret-" + id}
	if len(tags) > 0 {
		d.Metadata = map[string]interface{}{"tags": tags}
	}
	return d
}

func (f *byIDStore) GetByID(_ context.Context, id string) (*domain.Document, error) {
	f.getCalls++
	d, ok := f.docs[id]
	if !ok {
		return nil, nil
	}
	return &d, nil
}

func (f *byIDStore) GetBatch(_ context.Context, ids []string) ([]domain.Document, error) {
	out := make([]domain.Document, 0, len(ids))
	for _, id := range ids {
		if d, ok := f.docs[id]; ok {
			out = append(out, d)
		}
	}
	return out, nil
}

func (f *byIDStore) QueryByMetadata(_ context.Context, _ map[string]string, _ int) ([]domain.Document, error) {
	out := make([]domain.Document, 0, len(f.docs))
	for _, id := range []string{"pub", "sec"} { // deterministic order
		if d, ok := f.docs[id]; ok {
			out = append(out, d)
		}
	}
	return out, nil
}

func (f *byIDStore) GetStaleMemories(_ context.Context, _ int) ([]domain.Document, error) {
	return []domain.Document{f.docs["sec"]}, nil
}

func (f *byIDStore) Search(context.Context, []float32, domain.SearchOptions) ([]domain.SearchResult, error) {
	return nil, nil
}
func (f *byIDStore) Save(context.Context, *domain.Document) error        { return nil }
func (f *byIDStore) SaveBatch(context.Context, []*domain.Document) error { return nil }
func (f *byIDStore) Delete(context.Context, string) error                { return nil }
func (f *byIDStore) DeleteBatch(context.Context, []string) error         { return nil }
func (f *byIDStore) IncrementAccess(context.Context, string) error       { return nil }

func newByIDFixture() (*byIDStore, *authz.EnforcingVectorStore) {
	inner := &byIDStore{docs: map[string]domain.Document{
		"pub": tagged("pub"),
		"sec": tagged("sec", "internal"),
	}}
	return inner, authz.NewEnforcingVectorStore(inner, nil)
}

// customer is a surface that must not reach `internal`.
var customer = &domain.TagPredicate{ForbiddenTags: []string{"internal"}}

func TestGetByID_FiltersForbidden(t *testing.T) {
	t.Parallel()
	inner, s := newByIDFixture()
	ctx := domain.WithScope(context.Background(), customer)

	got, err := s.GetByID(ctx, "sec")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("forbidden document returned: %+v", got)
	}
	if inner.getCalls != 1 {
		t.Errorf("inner store should still be consulted (filtering, not short-circuit); calls=%d", inner.getCalls)
	}

	// Not over-blocking.
	ok, err := s.GetByID(ctx, "pub")
	if err != nil || ok == nil || ok.ID != "pub" {
		t.Errorf("permitted document was blocked: doc=%+v err=%v", ok, err)
	}
}

// A missing ctx predicate is a dropped-predicate bug, not an unscoped deployment —
// OSS supplies a bypass. It must fail LOUD rather than read unfiltered.
func TestGetByID_NoPredicateFailsClosed(t *testing.T) {
	t.Parallel()
	_, s := newByIDFixture()

	got, err := s.GetByID(context.Background(), "sec")
	if !errors.Is(err, authz.ErrScopeMissing) {
		t.Errorf("expected ErrScopeMissing, got err=%v doc=%+v", err, got)
	}
	if got != nil {
		t.Errorf("document returned despite missing predicate: %+v", got)
	}
}

func TestGetByID_BypassDelegates(t *testing.T) {
	t.Parallel()
	_, s := newByIDFixture()
	ctx := domain.WithScope(context.Background(), domain.ScopeSystem)

	got, err := s.GetByID(ctx, "sec")
	if err != nil || got == nil || got.ID != "sec" {
		t.Errorf("kernel-internal bypass should read through: doc=%+v err=%v", got, err)
	}
}

func TestGetBatch_FiltersForbidden(t *testing.T) {
	t.Parallel()
	_, s := newByIDFixture()
	ctx := domain.WithScope(context.Background(), customer)

	docs, err := s.GetBatch(ctx, []string{"pub", "sec"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(docs) != 1 || docs[0].ID != "pub" {
		t.Errorf("expected only the permitted doc, got %+v", docs)
	}
}

// QueryByMetadata is the primitive the experiential precedent lane reads through
// (precedent.go resolves an action path by plan_id), so it carries the same weight.
func TestQueryByMetadata_FiltersForbidden(t *testing.T) {
	t.Parallel()
	_, s := newByIDFixture()
	ctx := domain.WithScope(context.Background(), customer)

	docs, err := s.QueryByMetadata(ctx, map[string]string{"plan_id": "p1"}, 50)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(docs) != 1 || docs[0].ID != "pub" {
		t.Errorf("expected only the permitted doc, got %+v", docs)
	}
}

func TestQueryByMetadata_NoPredicateFailsClosed(t *testing.T) {
	t.Parallel()
	_, s := newByIDFixture()
	if _, err := s.QueryByMetadata(context.Background(), nil, 10); !errors.Is(err, authz.ErrScopeMissing) {
		t.Errorf("expected ErrScopeMissing, got %v", err)
	}
}

// The read tier is a domain.VectorReader and nothing else (ENG-1476). It used to
// carry Save/SaveBatch/Delete/DeleteBatch/IncrementAccess/GetStaleMemories as
// unenforced pass-throughs to the raw adapter, so anything holding the read
// chokepoint could write. If a write or maintenance method comes back, this test
// fails and the question "which chokepoint governs it?" has to be answered again.
func TestEnforcingVectorStore_IsReadOnly(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeOf((*authz.EnforcingVectorStore)(nil))
	if !typ.Implements(reflect.TypeOf((*domain.VectorReader)(nil)).Elem()) {
		t.Fatal("EnforcingVectorStore must implement domain.VectorReader")
	}
	for _, port := range []reflect.Type{
		reflect.TypeOf((*domain.VectorWriter)(nil)).Elem(),
		reflect.TypeOf((*domain.VectorMaintenance)(nil)).Elem(),
	} {
		if typ.Implements(port) {
			t.Errorf("EnforcingVectorStore implements %s: the read tier must not write or run maintenance", port)
		}
		for i := 0; i < port.NumMethod(); i++ {
			if _, ok := typ.MethodByName(port.Method(i).Name); ok {
				t.Errorf("EnforcingVectorStore has %s.%s", port.Name(), port.Method(i).Name)
			}
		}
	}
}

// The write tier is a domain.VectorWriter and nothing else: no read method, so it
// can never become a read path that skips EnforcingVectorStore.
func TestEnforcingStoreWriter_IsWriteOnly(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeOf((*authz.EnforcingStoreWriter)(nil))
	if !typ.Implements(reflect.TypeOf((*domain.VectorWriter)(nil)).Elem()) {
		t.Fatal("EnforcingStoreWriter must implement domain.VectorWriter")
	}
	for _, port := range []reflect.Type{
		reflect.TypeOf((*domain.VectorReader)(nil)).Elem(),
		reflect.TypeOf((*domain.VectorMaintenance)(nil)).Elem(),
	} {
		for i := 0; i < port.NumMethod(); i++ {
			if _, ok := typ.MethodByName(port.Method(i).Name); ok {
				t.Errorf("EnforcingStoreWriter has %s.%s", port.Name(), port.Method(i).Name)
			}
		}
	}
}
