package memory

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
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
	//
	// ENG-2115: fail closed — a document whose requested classification cannot be
	// carried in full (a tag outside the writer's write ceiling, or no ceiling) is
	// refused, never stored with those tags dropped. The error is both
	// ErrDocumentWriteDenied and the strict refusal (which names the tags and
	// unwraps to authz.ErrWriteDenied, so the ingest RPC maps it to InvalidArgument).
	final, dec, err := authz.ClassifyWriteStrict(ctx, w.authz, principal, domain.ClassificationHint(doc.Tags))
	if err != nil {
		w.logger.WarnContext(ctx, "authz: document write denied",
			slog.String("event", "authz_document_write_deny"),
			slog.String("principal", principal.String()),
			slog.String("document", doc.ID),
			slog.String("reason", string(dec.Reason)),
			slog.String("detail", dec.Detail),
			slog.String("error", err.Error()))
		return nil, fmt.Errorf("%w: %w", ErrDocumentWriteDenied, err)
	}
	// ADR-0142 D12: private to the conversation owner the work is for.
	doc.Tags = domain.StampPrivateOwner(ctx, final)
	return w.inner.SaveDocument(ctx, doc)
}
