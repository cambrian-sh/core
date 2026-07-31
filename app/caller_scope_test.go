package app

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

type scopeAuthz struct {
	pred   *domain.TagPredicate
	reason domain.DecisionReason
	gotRef domain.PrincipalRef
	gotSf  domain.SurfaceRef
}

func (a *scopeAuthz) Authorize(context.Context, domain.AccessRequest) domain.AccessDecision {
	return domain.AccessDecision{}
}

func (a *scopeAuthz) Filter(context.Context, domain.PrincipalRef, domain.SurfaceRef, domain.ResourceKind, []domain.Taggable) ([]domain.Taggable, []domain.AccessDecision) {
	return nil, nil
}

func (a *scopeAuthz) ReadFilter(_ context.Context, p domain.PrincipalRef, s domain.SurfaceRef) (*domain.TagPredicate, domain.AccessDecision) {
	a.gotRef, a.gotSf = p, s
	return a.pred, domain.AccessDecision{Reason: a.reason}
}

func (a *scopeAuthz) ClassifyWrite(context.Context, domain.PrincipalRef, []string) ([]string, domain.AccessDecision) {
	return nil, domain.AccessDecision{}
}

// The point of the whole change: a session opened by an entitled caller carries
// that caller's scope, so later turns can compute effective = caller ∩ agent.
//
// Before this, every session was opened with the unscoped constructor, so
// caller_scope was always empty and the intersection reduced to the agent's own
// scope — the mechanism existed end to end and was never populated.
func TestCallerScope_PersistsTheResolvedTerm(t *testing.T) {
	az := &scopeAuthz{pred: &domain.TagPredicate{
		RequiredTags:  []string{"finance"},
		ForbiddenTags: []string{"secrets"},
	}}
	got := callerScopeForPrincipal(context.Background(), az, "alice",
		domain.SurfaceRef{ID: "console", Kind: domain.SurfaceOperator})

	if len(got.RequiredTags) != 1 || got.RequiredTags[0] != "finance" {
		t.Fatalf("required = %v", got.RequiredTags)
	}
	if len(got.ForbiddenTags) != 1 || got.ForbiddenTags[0] != "secrets" {
		t.Fatalf("forbidden = %v", got.ForbiddenTags)
	}
	// Asked as a USER, on the surface the kernel stamped — not as an agent, and not
	// on a surface the caller supplied.
	if az.gotRef.Kind != domain.PrincipalUser || az.gotRef.ID != "alice" {
		t.Errorf("asked as %+v, want the user principal", az.gotRef)
	}
	if az.gotSf.Kind != domain.SurfaceOperator {
		t.Errorf("surface = %q, want the stamped one", az.gotSf.Kind)
	}
}

// A predicate that cannot be represented without WIDENING is dropped, not
// flattened. Multi-clause CNF flattened into one OR-set would grant the caller
// access the decision point refused.
func TestCallerScope_NeverWidens(t *testing.T) {
	az := &scopeAuthz{pred: &domain.TagPredicate{
		AnyOfClauses: [][]string{{"a", "b"}, {"c", "d"}},
	}}
	got := callerScopeForPrincipal(context.Background(), az, "alice", domain.SurfaceRef{})
	if !got.IsZero() {
		t.Fatalf("multi-clause CNF produced %+v; flattening it widens the caller's scope", got)
	}
}

// "No read authorized" must not become "no constraint" — that inverts it. The
// session still opens (the agent's own scope governs) but carries no term.
func TestCallerScope_NilPredicateDoesNotBecomeUnrestricted(t *testing.T) {
	az := &scopeAuthz{pred: nil, reason: domain.ReasonNoPrincipal}
	got := callerScopeForPrincipal(context.Background(), az, "ghost", domain.SurfaceRef{})
	if !got.IsZero() {
		t.Fatalf("nil predicate produced %+v", got)
	}
}

// Bypass (an unscoped deployment, or the OSS AllowAllAuthorizer) is faithfully
// "no constraint" — this is what keeps the change a no-op where no policy exists.
func TestCallerScope_BypassIsNoConstraint(t *testing.T) {
	az := &scopeAuthz{pred: &domain.TagPredicate{Bypass: true}}
	if got := callerScopeForPrincipal(context.Background(), az, "alice", domain.SurfaceRef{}); !got.IsZero() {
		t.Fatalf("bypass produced %+v, want an empty term", got)
	}
}

// No authorizer and no authenticated principal are both "nothing to scope to".
// Inventing an identity here would be worse than leaving the term empty.
func TestCallerScope_NoAuthorizerOrNoPrincipal(t *testing.T) {
	if got := callerScopeFor(context.Background(), nil); !got.IsZero() {
		t.Fatalf("nil authorizer produced %+v", got)
	}
	az := &scopeAuthz{pred: &domain.TagPredicate{RequiredTags: []string{"finance"}}}
	// No operator principal on the context: the interceptor never ran.
	if got := callerScopeFor(context.Background(), az); !got.IsZero() {
		t.Fatalf("unauthenticated context produced %+v — an identity was invented", got)
	}
}
