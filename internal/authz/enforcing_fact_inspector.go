package authz

import (
	"context"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
)

// EnforcingFactInspector is the fail-closed decorator over the operator plane's fact
// inspection (domain.FactInspector; T-20261103-operator-plane-cannot-inspect-facts-no-rpc).
// Every content read needs a predicate — the explicit one, else the one on ctx — and
// refuses with ErrScopeMissing when there is none, exactly like EnforcingFactItemReader;
// the inner reader applies it IN SQL. It is composed in kernel.NewMemoryStack with the
// other fact-layer chokepoints and nowhere else.
type EnforcingFactInspector struct {
	inner  domain.FactInspector
	reader *EnforcingFactItemReader // shares the resolve rule and its deny log
}

var _ domain.FactInspector = (*EnforcingFactInspector)(nil)

// NewEnforcingFactInspector wraps inner.
func NewEnforcingFactInspector(inner domain.FactInspector, logger *slog.Logger) *EnforcingFactInspector {
	return &EnforcingFactInspector{inner: inner, reader: NewEnforcingFactItemReader(nil, logger)}
}

// ListFacts — ENFORCED.
func (r *EnforcingFactInspector) ListFacts(ctx context.Context, f domain.FactListFilter, scope *domain.TagPredicate) ([]domain.Fact, string, error) {
	eff, err := r.reader.resolve(ctx, scope, "ListFacts")
	if err != nil {
		return nil, "", err
	}
	return r.inner.ListFacts(ctx, f, eff)
}

// FactsByID — ENFORCED (a by-id read, AGENTS.md §2.4).
func (r *EnforcingFactInspector) FactsByID(ctx context.Context, ids []domain.KnowledgeItemID, scope *domain.TagPredicate) ([]domain.Fact, error) {
	eff, err := r.reader.resolve(ctx, scope, "InspectFactsByID")
	if err != nil {
		return nil, err
	}
	return r.inner.FactsByID(ctx, ids, eff)
}

// FactChain — ENFORCED.
func (r *EnforcingFactInspector) FactChain(ctx context.Context, f domain.Fact, scope *domain.TagPredicate) ([]domain.Fact, error) {
	eff, err := r.reader.resolve(ctx, scope, "InspectFactChain")
	if err != nil {
		return nil, err
	}
	return r.inner.FactChain(ctx, f, eff)
}

// FactCoverage — PASS-THROUGH. It returns aggregate counts only (documents, markers,
// items), never an item, a value or a tag, so there is no row to filter; the operator
// plane's role gate is its access control.
func (r *EnforcingFactInspector) FactCoverage(ctx context.Context) (domain.FactCoverage, error) {
	return r.inner.FactCoverage(ctx)
}
