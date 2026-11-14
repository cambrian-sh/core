package authz_test

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

type recordingInspector struct{ scopes []*domain.TagPredicate }

func (r *recordingInspector) ListFacts(_ context.Context, _ domain.FactListFilter, s *domain.TagPredicate) ([]domain.Fact, string, error) {
	r.scopes = append(r.scopes, s)
	return []domain.Fact{{ID: "f1"}}, "", nil
}
func (r *recordingInspector) FactsByID(_ context.Context, _ []domain.KnowledgeItemID, s *domain.TagPredicate) ([]domain.Fact, error) {
	r.scopes = append(r.scopes, s)
	return nil, nil
}
func (r *recordingInspector) FactChain(_ context.Context, _ domain.Fact, s *domain.TagPredicate) ([]domain.Fact, error) {
	r.scopes = append(r.scopes, s)
	return nil, nil
}
func (r *recordingInspector) FactCoverage(context.Context) (domain.FactCoverage, error) {
	return domain.FactCoverage{FactItems: 7}, nil
}

// The operator plane's fact inspection is fail-closed like every fact read
// (T-20261103-operator-plane-cannot-inspect-facts-no-rpc): no predicate on the call
// or ctx refuses with ErrScopeMissing and never reaches the store; an explicit or
// ctx predicate is handed through; coverage (counts only) passes through.
func TestEnforcingFactInspector_FailsClosedWithoutScope(t *testing.T) {
	inner := &recordingInspector{}
	fi := authz.NewEnforcingFactInspector(inner, nil)
	ctx := context.Background()
	if _, _, err := fi.ListFacts(ctx, domain.FactListFilter{}, nil); !errors.Is(err, authz.ErrScopeMissing) {
		t.Fatalf("ListFacts with no scope: %v", err)
	}
	if _, err := fi.FactsByID(ctx, nil, nil); !errors.Is(err, authz.ErrScopeMissing) {
		t.Fatalf("FactsByID with no scope: %v", err)
	}
	if _, err := fi.FactChain(ctx, domain.Fact{}, nil); !errors.Is(err, authz.ErrScopeMissing) {
		t.Fatalf("FactChain with no scope: %v", err)
	}
	if len(inner.scopes) != 0 {
		t.Fatalf("a refused read reached the store %d times", len(inner.scopes))
	}
	if _, _, err := fi.ListFacts(ctx, domain.FactListFilter{}, domain.ScopeSystem); err != nil || inner.scopes[0] != domain.ScopeSystem {
		t.Fatalf("explicit scope: err %v scopes %v", err, inner.scopes)
	}
	ctxScope := &domain.TagPredicate{RequiredTags: []string{"team:a"}}
	if _, err := fi.FactChain(domain.WithScope(ctx, ctxScope), domain.Fact{}, nil); err != nil || inner.scopes[1] != ctxScope {
		t.Fatalf("ctx scope: err %v scopes %v", err, inner.scopes)
	}
	if c, err := fi.FactCoverage(ctx); err != nil || c.FactItems != 7 {
		t.Fatalf("coverage: %+v %v", c, err)
	}
}
