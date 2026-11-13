package authz

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

type fakeOntology struct{ got *domain.TagPredicate }

func (f *fakeOntology) Current(string) *domain.OntologySnapshot { return nil }
func (f *fakeOntology) Apply(context.Context, string, domain.OntologyDoc, string, bool) (domain.OntologyApplyResult, error) {
	return domain.OntologyApplyResult{}, nil
}
func (f *fakeOntology) Violations(_ context.Context, scope *domain.TagPredicate, _ string, _ int) ([]domain.OntologyViolation, bool, error) {
	f.got = scope
	return nil, false, nil
}

type denyAll struct{ domain.AllowAllAuthorizer }

func (denyAll) ReadFilter(context.Context, domain.PrincipalRef, domain.SurfaceRef) (*domain.TagPredicate, domain.AccessDecision) {
	return nil, domain.AccessDecision{Reason: "no grant"}
}

// The violations report runs AS a principal: the predicate comes from the
// authorizer, never from the caller, and no predicate fails closed.
func TestScopedOntology_ResolvesScopeAndFailsClosed(t *testing.T) {
	f := &fakeOntology{}
	if _, _, err := ScopedOntology(nil, f, nil).Violations(context.Background(), domain.SystemPrincipal, "", 0); err != nil || f.got == nil {
		t.Fatalf("allow-all: predicate %v, err %v", f.got, err)
	}
	f.got = nil
	_, _, err := ScopedOntology(denyAll{}, f, nil).Violations(context.Background(), domain.SystemPrincipal, "", 0)
	if !errors.Is(err, domain.ErrQueryDenied) || f.got != nil {
		t.Fatalf("denied principal reached the store (scope %v) or was not refused: %v", f.got, err)
	}
}
