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
		pred, err := readScope(ctx, authorizer, principal, log, "knowledge query")
		if err != nil {
			return domain.QueryResult{}, err
		}
		return plane.Query(ctx, q, pred)
	}
}

// readScope resolves principal's read predicate from the effective Authorizer and
// the transport surface on the context. No predicate fails CLOSED with
// domain.ErrQueryDenied — the one refusal every principal-taking seam here gives,
// so a caller cannot use one seam to learn what another withholds.
func readScope(ctx context.Context, authorizer domain.Authorizer, principal domain.PrincipalRef, log *slog.Logger, what string) (*domain.TagPredicate, error) {
	surface := domain.SurfaceFromContext(ctx)
	pred, dec := authorizer.ReadFilter(ctx, principal, surface)
	if pred == nil {
		if log != nil {
			log.Warn(what+" denied: no read predicate",
				"principal", principal.ID, "principal_kind", string(principal.Kind),
				"surface", string(surface.Kind), "reason", string(dec.Reason))
		}
		return nil, fmt.Errorf("%w: %s", domain.ErrQueryDenied, dec.Reason)
	}
	return pred, nil
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
		pred, err := readScope(ctx, authorizer, principal, log, "knowledge catalog")
		if err != nil {
			return nil, err
		}
		return src.Catalog(pred), nil
	}
}

// OntologyStore is the live ontology as the kernel holds it (internal/ontology):
// its violations report takes a predicate, which no plugin may choose.
type OntologyStore interface {
	Current(namespace string) *domain.OntologySnapshot
	Apply(ctx context.Context, namespace string, change domain.OntologyDoc, author string, dryRun bool) (domain.OntologyApplyResult, error)
	Violations(ctx context.Context, scope *domain.TagPredicate, namespace string, limit int) ([]domain.OntologyViolation, bool, error)
}

// ScopedOntology is the plugin seam over the live ontology (ADR-0149 D2): the
// violations report runs AS a principal, its predicate resolved here exactly as
// the query and catalog seams resolve theirs. Reads of the declarations and
// Apply pass through — who may apply is the operator plane's decision (an
// Operator-only RPC, audited), and declarations carry no rows.
func ScopedOntology(authorizer domain.Authorizer, o OntologyStore, log *slog.Logger) domain.Ontology {
	if authorizer == nil {
		authorizer = domain.AllowAllAuthorizer{}
	}
	return scopedOntology{OntologyStore: o, authorizer: authorizer, log: log}
}

type scopedOntology struct {
	OntologyStore
	authorizer domain.Authorizer
	log        *slog.Logger
}

func (s scopedOntology) Violations(ctx context.Context, principal domain.PrincipalRef, namespace string, limit int) ([]domain.OntologyViolation, bool, error) {
	pred, err := readScope(ctx, s.authorizer, principal, s.log, "ontology violations")
	if err != nil {
		return nil, false, err
	}
	return s.OntologyStore.Violations(ctx, pred, namespace, limit)
}

// MayReadEvidence reports whether principal may read the evidence row ev, by the
// same predicate the knowledge query seam resolves for that principal
// (readScope: the effective Authorizer and the transport surface on ctx) and the
// same row test the query plane's node guard applies (TagPredicate.AllowsRow over
// the row's classification and parties). A principal with no read predicate, or a
// nil row, may read nothing.
//
// A write that cites evidence as its support (an agent's knowledge or link
// proposal) uses it so that a principal cannot attach claims to evidence it could
// not read, nor learn which evidence ids exist
// (T-20261107-propose-knowledge-link-proposals-do-not-check).
func MayReadEvidence(ctx context.Context, authorizer domain.Authorizer, principal domain.PrincipalRef, ev *domain.Evidence, log *slog.Logger) bool {
	if ev == nil {
		return false
	}
	if authorizer == nil {
		authorizer = domain.AllowAllAuthorizer{}
	}
	pred, err := readScope(ctx, authorizer, principal, log, "evidence citation")
	if err != nil {
		return false
	}
	if pred.Unfiltered() {
		return true
	}
	return pred.AllowsRow(ev.Classification, ev.Parties)
}
