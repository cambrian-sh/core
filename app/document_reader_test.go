package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// stubStore records the CONTEXT it was called with, because the scope the
// enforcing decorator resolves against travels there.
type stubStore struct {
	domain.VectorStore
	gotCtx context.Context
	doc    *domain.Document
	err    error
	calls  int
}

func (s *stubStore) GetByID(ctx context.Context, _ string) (*domain.Document, error) {
	s.calls++
	s.gotCtx = ctx
	return s.doc, s.err
}

func TestKernelDocumentReader_DelegatesToTheEnforcingStore(t *testing.T) {
	st := &stubStore{doc: &domain.Document{ID: "d1", Text: "body"}}
	r := &kernelDocumentReader{store: st}

	got, err := r.GetDocument(context.Background(), domain.SystemPrincipal, "d1")
	if err != nil {
		t.Fatalf("GetDocument: %v", err)
	}
	if st.calls != 1 {
		t.Fatalf("expected one delegated read, got %d", st.calls)
	}
	if got.Text != "body" {
		t.Fatalf("body not returned: %q", got.Text)
	}
}

// The whole point of taking a principal: the scope must reach the decorator, or
// the read is enforced against whatever the context happened to carry.
func TestKernelDocumentReader_StampsTheScopeFromThePrincipal(t *testing.T) {
	st := &stubStore{doc: &domain.Document{ID: "d1"}}
	r := &kernelDocumentReader{store: st}

	if _, err := r.GetDocument(context.Background(), domain.SystemPrincipal, "d1"); err != nil {
		t.Fatalf("GetDocument: %v", err)
	}
	if st.gotCtx == nil {
		t.Fatal("the store was called with no context")
	}
	if _, ok := domain.ScopeFromContext(st.gotCtx); !ok {
		t.Fatal("no scope was stamped on the context; the enforcing store would " +
			"resolve against whatever the caller happened to carry")
	}
}

// nil from the enforcing store means absent OR unreadable — the decorator makes
// them indistinguishable on purpose, and this must not turn one into a different
// answer than the other.
func TestKernelDocumentReader_NilBecomesNotFound(t *testing.T) {
	r := &kernelDocumentReader{store: &stubStore{doc: nil}}

	_, err := r.GetDocument(context.Background(), domain.SystemPrincipal, "gone")
	if !errors.Is(err, domain.ErrDocumentNotFound) {
		t.Fatalf("a filtered/absent read produced %v", err)
	}
}

// A real store failure must stay an error. Collapsing it into not-found would
// report "this document does not exist" when the truth is "the database is down".
func TestKernelDocumentReader_StoreFailureIsNotMissing(t *testing.T) {
	boom := errors.New("connection refused")
	r := &kernelDocumentReader{store: &stubStore{err: boom}}

	_, err := r.GetDocument(context.Background(), domain.SystemPrincipal, "d1")
	if errors.Is(err, domain.ErrDocumentNotFound) {
		t.Fatal("a store failure was reported as a missing document")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("the underlying failure was lost: %v", err)
	}
}

func TestKernelDocumentReader_NoStoreIsAnHonestError(t *testing.T) {
	r := &kernelDocumentReader{}
	_, err := r.GetDocument(context.Background(), domain.SystemPrincipal, "d1")
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("expected a stated absence, got %v", err)
	}
}

func TestKernelDocumentReader_EmptyIDIsRejectedBeforeReading(t *testing.T) {
	st := &stubStore{}
	r := &kernelDocumentReader{store: st}
	if _, err := r.GetDocument(context.Background(), domain.SystemPrincipal, ""); err == nil {
		t.Fatal("an empty id was accepted")
	}
	if st.calls != 0 {
		t.Fatal("the store was reached with an empty id")
	}
}

// policyReadAuthorizer grants each principal kind a fixed read predicate (nil =
// no read), so the reader's principal → predicate resolution is observable.
type policyReadAuthorizer struct {
	domain.AllowAllAuthorizer
	byKind       map[domain.PrincipalKind]*domain.TagPredicate
	gotSurface   domain.SurfaceRef
	gotPrincipal domain.PrincipalRef
}

func (a *policyReadAuthorizer) ReadFilter(_ context.Context, p domain.PrincipalRef, s domain.SurfaceRef) (*domain.TagPredicate, domain.AccessDecision) {
	a.gotPrincipal, a.gotSurface = p, s
	pred := a.byKind[p.Kind]
	if pred == nil {
		return nil, domain.AccessDecision{Reason: domain.ReasonNoPrincipal}
	}
	return pred, domain.AccessDecision{Allowed: true}
}

// ENG-1610 regression: through the REAL enforcing decorator, a non-system principal
// must read what its predicate admits. Before the fix the reader stamped only the
// principal, the decorator found no predicate on ctx, and every such read failed
// with authz.ErrScopeMissing (MCP get_document and the plugin DocReader were broken).
func TestKernelDocumentReader_NonSystemPrincipalReadsThroughEnforcingStore(t *testing.T) {
	inner := &stubStore{doc: &domain.Document{ID: "d1", Text: "body",
		Metadata: map[string]interface{}{"tags": []string{"team:a"}}}}
	az := &policyReadAuthorizer{byKind: map[domain.PrincipalKind]*domain.TagPredicate{
		domain.PrincipalAgent: {ForbiddenTags: []string{"secret"}},
	}}
	r := newKernelDocumentReader(authz.NewEnforcingVectorStore(inner, nil), az)

	ctx := domain.WithSurface(context.Background(), domain.SurfaceRef{Kind: domain.SurfaceMCP})
	got, err := r.GetDocument(ctx, domain.AgentPrincipal("mcp:client"), "d1")
	if err != nil {
		t.Fatalf("a permitted non-system read failed: %v", err)
	}
	if got.Text != "body" {
		t.Fatalf("body not returned: %q", got.Text)
	}
	if az.gotSurface.Kind != domain.SurfaceMCP || az.gotPrincipal.ID != "mcp:client" {
		t.Fatalf("predicate resolved for %v on %v, want mcp:client on the ctx surface", az.gotPrincipal, az.gotSurface)
	}
}

// The predicate the decision point grants is the one applied: a row it forbids
// comes back as not-found, indistinguishable from an absent row.
func TestKernelDocumentReader_PredicateFiltersThroughEnforcingStore(t *testing.T) {
	inner := &stubStore{doc: &domain.Document{ID: "d1",
		Metadata: map[string]interface{}{"tags": []string{"secret"}}}}
	az := &policyReadAuthorizer{byKind: map[domain.PrincipalKind]*domain.TagPredicate{
		domain.PrincipalAgent: {ForbiddenTags: []string{"secret"}},
	}}
	r := newKernelDocumentReader(authz.NewEnforcingVectorStore(inner, nil), az)

	_, err := r.GetDocument(context.Background(), domain.AgentPrincipal("a1"), "d1")
	if !errors.Is(err, domain.ErrDocumentNotFound) {
		t.Fatalf("a forbidden row produced %v, want ErrDocumentNotFound", err)
	}
}

// A principal granted no read never reaches the store, and the refusal is the same
// not-found an absent document gives. A nil authorizer refuses too (never allow-all).
func TestKernelDocumentReader_NoReadGrantedIsNotFound(t *testing.T) {
	for name, az := range map[string]domain.Authorizer{
		"no predicate":   &policyReadAuthorizer{},
		"nil authorizer": nil,
	} {
		t.Run(name, func(t *testing.T) {
			inner := &stubStore{doc: &domain.Document{ID: "d1"}}
			r := newKernelDocumentReader(authz.NewEnforcingVectorStore(inner, nil), az)
			_, err := r.GetDocument(context.Background(), domain.AgentPrincipal("a1"), "d1")
			if !errors.Is(err, domain.ErrDocumentNotFound) {
				t.Fatalf("got %v, want ErrDocumentNotFound", err)
			}
			if inner.calls != 0 {
				t.Fatal("the store was reached for a principal granted no read")
			}
		})
	}
}
