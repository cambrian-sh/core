package memory

import (
	"context"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
)

// EnforcingDocumentStore is the write-side chokepoint for the DOCUMENT ENTITY.
//
// ADR-0093 made `documents.tags` the authoritative classification — the value every
// access decision about that document's chunks now reads. ADR-0085's rule is that no
// principal holds a raw store reference and the check runs on EVERY write path; when
// the document entity was introduced it was wired straight to the adapter, which
// quietly exempted the one column that matters most from that rule.
//
// So the same discipline the vector store has applies here: the tags an ingesting
// agent supplies are a REQUEST, and the decision point returns the answer. Without
// this, an agent could classify a document however it liked simply by ingesting it,
// and every chunk under that document would inherit the classification.
type EnforcingDocumentStore struct {
	inner  domain.DocumentStore
	authz  domain.Authorizer
	logger *slog.Logger
}

// NewEnforcingDocumentStore wraps a document store with write classification. A nil
// authorizer falls back to the OSS allow-all default, matching the vector store's
// behaviour — OSS fails open, the premium policy plugin fails closed.
func NewEnforcingDocumentStore(inner domain.DocumentStore, a domain.Authorizer, logger *slog.Logger) *EnforcingDocumentStore {
	if a == nil {
		a = domain.AllowAllAuthorizer{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &EnforcingDocumentStore{inner: inner, authz: a, logger: logger}
}

// SaveDocument classifies the document's tags, then delegates.
func (w *EnforcingDocumentStore) SaveDocument(ctx context.Context, doc domain.SourceDocument) ([]string, error) {
	principal := domain.PrincipalFromContext(ctx)
	// ADR-0099: identity is not a classification. The caller's list carries both, and
	// a controlled vocabulary rejects an identity term as coinage — which denied the
	// whole write and made a document unwritable for carrying its own name.
	final, dec := w.authz.ClassifyWrite(ctx, principal, domain.ClassificationHint(doc.Tags))
	if !dec.Allowed {
		w.logger.WarnContext(ctx, "authz: document write denied",
			slog.String("event", "authz_document_write_deny"),
			slog.String("principal", principal.String()),
			slog.String("document", doc.ID),
			slog.String("reason", string(dec.Reason)),
			slog.String("detail", dec.Detail))
		return nil, ErrDocumentWriteDenied
	}
	doc.Tags = final
	return w.inner.SaveDocument(ctx, doc)
}
