package authz

import (
	"context"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
)

// ObjectsStore is the object tables' reader as the kernel holds it
// (internal/objects.Reader): it takes a predicate, which no plugin may choose.
type ObjectsStore interface {
	List(ctx context.Context, scope *domain.TagPredicate, q domain.ObjectsQuery) (domain.ObjectsResult, error)
	Get(ctx context.Context, scope *domain.TagPredicate, namespace, entityID string) (domain.ObjectView, error)
}

// ScopedObjects is the plugin seam over the object tables (ADR-0149 D6, D7):
// ListObjects and GetObject run AS a principal, its read predicate resolved here
// exactly as the query, catalog and ontology seams resolve theirs (readScope —
// no predicate fails closed with ErrQueryDenied).
func ScopedObjects(authorizer domain.Authorizer, store ObjectsStore, log *slog.Logger) domain.Objects {
	if authorizer == nil {
		authorizer = domain.AllowAllAuthorizer{}
	}
	return scopedObjects{store: store, authorizer: authorizer, log: log}
}

type scopedObjects struct {
	store      ObjectsStore
	authorizer domain.Authorizer
	log        *slog.Logger
}

func (s scopedObjects) ListObjects(ctx context.Context, principal domain.PrincipalRef, q domain.ObjectsQuery) (domain.ObjectsResult, error) {
	pred, err := readScope(ctx, s.authorizer, principal, s.log, "list objects")
	if err != nil {
		return domain.ObjectsResult{}, err
	}
	return s.store.List(ctx, pred, q)
}

func (s scopedObjects) GetObject(ctx context.Context, principal domain.PrincipalRef, namespace, entityID string) (domain.ObjectView, error) {
	pred, err := readScope(ctx, s.authorizer, principal, s.log, "get object")
	if err != nil {
		return domain.ObjectView{}, err
	}
	return s.store.Get(ctx, pred, namespace, entityID)
}
