package authz

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

type fakeObjects struct{ got *domain.TagPredicate }

func (f *fakeObjects) List(_ context.Context, scope *domain.TagPredicate, _ domain.ObjectsQuery) (domain.ObjectsResult, error) {
	f.got = scope
	return domain.ObjectsResult{}, nil
}

func (f *fakeObjects) Get(_ context.Context, scope *domain.TagPredicate, _, _ string) (domain.ObjectView, error) {
	f.got = scope
	return domain.ObjectView{}, nil
}

// ListObjects and GetObject run AS a principal: the predicate comes from the
// authorizer, never from the caller, and no predicate fails closed before the
// store is reached.
func TestScopedObjects_ResolvesScopeAndFailsClosed(t *testing.T) {
	ctx := context.Background()
	f := &fakeObjects{}
	s := ScopedObjects(nil, f, nil)
	if _, err := s.ListObjects(ctx, domain.SystemPrincipal, domain.ObjectsQuery{}); err != nil || f.got == nil {
		t.Fatalf("allow-all list: predicate %v, err %v", f.got, err)
	}
	f.got = nil
	if _, err := s.GetObject(ctx, domain.SystemPrincipal, "", "x/1"); err != nil || f.got == nil {
		t.Fatalf("allow-all get: predicate %v, err %v", f.got, err)
	}
	f.got = nil
	denied := ScopedObjects(denyAll{}, f, nil)
	if _, err := denied.ListObjects(ctx, domain.AgentPrincipal("a"), domain.ObjectsQuery{}); !errors.Is(err, domain.ErrQueryDenied) || f.got != nil {
		t.Fatalf("denied list reached the store (%v) or was not refused: %v", f.got, err)
	}
	if _, err := denied.GetObject(ctx, domain.AgentPrincipal("a"), "", "x/1"); !errors.Is(err, domain.ErrQueryDenied) || f.got != nil {
		t.Fatalf("denied get reached the store (%v) or was not refused: %v", f.got, err)
	}
}
