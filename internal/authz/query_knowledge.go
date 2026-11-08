// Knowledge-query enforcement point (ADR-0118 D1).
//
// The seam handed to plugins takes a PRINCIPAL, never a predicate — a seam
// accepting a *TagPredicate would let the caller choose its own access scope
// (a bypass with extra steps; the document_reader precedent). The predicate is
// resolved HERE, from the effective Authorizer and the transport-derived
// surface already on the context, and the plane below treats scope as a
// required positional parameter (nil denies).
package authz

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
)

// QueryKnowledgeFunc builds the principal-resolving wrapper over the query
// plane. OSS default authorizer (AllowAllAuthorizer) yields the bypass
// predicate — an unscoped deployment reads exactly as before; a policy
// authorizer that grants the principal no read predicate fails CLOSED with
// domain.ErrQueryDenied.
func QueryKnowledgeFunc(authorizer domain.Authorizer, plane domain.QueryPlane, log *slog.Logger) func(context.Context, domain.PrincipalRef, domain.KnowledgeQuery) (domain.QueryResult, error) {
	if authorizer == nil {
		authorizer = domain.AllowAllAuthorizer{}
	}
	return func(ctx context.Context, principal domain.PrincipalRef, q domain.KnowledgeQuery) (domain.QueryResult, error) {
		surface := domain.SurfaceFromContext(ctx)
		pred, dec := authorizer.ReadFilter(ctx, principal, surface)
		if pred == nil {
			if log != nil {
				log.Warn("knowledge query denied: no read predicate",
					"principal", principal.ID, "principal_kind", string(principal.Kind),
					"surface", string(surface.Kind), "reason", string(dec.Reason))
			}
			return domain.QueryResult{}, fmt.Errorf("%w: %s", domain.ErrQueryDenied, dec.Reason)
		}
		return plane.Query(ctx, q, pred)
	}
}

// CatalogSource is what KnowledgeCatalogFunc binds: the query plane's
// names-and-counts surface, scoped by a predicate the caller never chooses.
type CatalogSource interface {
	Catalog(scope *domain.TagPredicate) domain.SubstrateCatalog
}

// KnowledgeCatalogFunc is QueryKnowledgeFunc for the substrate catalog
// (domain.SubstrateCatalog): principal in, the predicate resolved here from the
// effective Authorizer and the transport surface on the context, a catalog BOUND
// to that predicate out. No read predicate fails closed with ErrQueryDenied, the
// same refusal the query seam gives — so a caller cannot use one seam to learn
// what the other withholds.
func KnowledgeCatalogFunc(authorizer domain.Authorizer, src CatalogSource, log *slog.Logger) func(context.Context, domain.PrincipalRef) (domain.SubstrateCatalog, error) {
	if authorizer == nil {
		authorizer = domain.AllowAllAuthorizer{}
	}
	return func(ctx context.Context, principal domain.PrincipalRef) (domain.SubstrateCatalog, error) {
		surface := domain.SurfaceFromContext(ctx)
		pred, dec := authorizer.ReadFilter(ctx, principal, surface)
		if pred == nil {
			if log != nil {
				log.Warn("knowledge catalog denied: no read predicate",
					"principal", principal.ID, "principal_kind", string(principal.Kind),
					"surface", string(surface.Kind), "reason", string(dec.Reason))
			}
			return nil, fmt.Errorf("%w: %s", domain.ErrQueryDenied, dec.Reason)
		}
		return src.Catalog(pred), nil
	}
}
