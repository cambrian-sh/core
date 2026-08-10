package authz

import (
	"context"
	"errors"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
)

// ErrWriteDenied is returned when the decision point refuses a write outright
// (e.g. a tag outside the controlled vocabulary). The RPC boundary maps it to
// InvalidArgument. The specific reason travels in the AccessDecision, which the
// chokepoint logs — the error itself is deliberately coarse so it cannot be used
// to probe the vocabulary.
var ErrWriteDenied = errors.New("authz: write denied by policy")

// EnforcingStoreWriter is the write-side chokepoint. EVERY write — RPC and
// in-process, including LLM-driven ones — passes through it; no principal holds a
// raw store reference. There is no "trusted in-process" carve-out: process
// membership does not constrain a model's output, so the check runs on every path.
//
// On each write it asks the decision point to CLASSIFY the write: the agent's own
// tags are a request, never the answer. The returned classification replaces
// whatever the writer put on the document. In OSS the decision point returns the
// authored tags unchanged (unscoped deployment); in a premium deployment it
// derives them from the principal's operator-configured write classification,
// narrowed (never broadened) by the writer's hint, and stamps provenance.
//
// Reads and other methods pass through the embedded store unchanged.
type EnforcingStoreWriter struct {
	domain.VectorStore
	authz  domain.Authorizer
	logger *slog.Logger
}

// NewEnforcingStoreWriter wraps inner with write-side enforcement. A nil
// authorizer falls back to the OSS allow-all default; a nil logger to slog.Default().
func NewEnforcingStoreWriter(inner domain.VectorStore, a domain.Authorizer, logger *slog.Logger) *EnforcingStoreWriter {
	if a == nil {
		a = domain.AllowAllAuthorizer{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &EnforcingStoreWriter{VectorStore: inner, authz: a, logger: logger}
}

// Save classifies and stamps a single document, then delegates.
func (w *EnforcingStoreWriter) Save(ctx context.Context, doc *domain.Document) error {
	if err := w.classify(ctx, doc); err != nil {
		return err
	}
	return w.VectorStore.Save(ctx, doc)
}

// SaveBatch classifies and stamps every document, then delegates. A single
// rejection fails the whole batch (fail-closed).
func (w *EnforcingStoreWriter) SaveBatch(ctx context.Context, docs []*domain.Document) error {
	for _, doc := range docs {
		if err := w.classify(ctx, doc); err != nil {
			return err
		}
	}
	return w.VectorStore.SaveBatch(ctx, docs)
}

// classify replaces the document's authored tags with the decision point's
// authoritative classification.
func (w *EnforcingStoreWriter) classify(ctx context.Context, doc *domain.Document) error {
	if doc == nil {
		return nil
	}
	principal := domain.PrincipalFromContext(ctx)
	// ADR-0099: strip identity before classification. See domain.ClassificationHint —
	// an identity term reaching a controlled vocabulary is rejected as coinage, which
	// denies the write outright rather than ignoring a term that was never a label.
	final, dec := w.authz.ClassifyWrite(ctx, principal, domain.ClassificationHint(DocTags(doc)))
	if !dec.Allowed {
		w.logger.WarnContext(ctx, "authz: write denied",
			slog.String("event", "authz_write_deny"),
			slog.String("principal", principal.String()),
			slog.String("reason", string(dec.Reason)),
			slog.String("detail", dec.Detail))
		return ErrWriteDenied
	}
	// A write that ends up with no classification at all is readable only by an
	// unrestricted or bypass reader. That fails safe on confidentiality but is a
	// silent visibility surprise for the author, so it is surfaced (INV-3). The
	// decision point emits the richer version of this warning — it is the only
	// party that can tell a classification tag from a provenance stamp.
	if len(final) == 0 {
		w.logger.WarnContext(ctx, "authz: write carries no classification",
			slog.String("event", "authz_write_unclassified"),
			slog.String("principal", principal.String()),
			slog.Any("requested", DocTags(doc)))
	}
	if doc.Metadata == nil {
		doc.Metadata = make(map[string]interface{})
	}
	doc.Metadata["tags"] = final
	return nil
}

// DocTags extracts a document's classification tags, handling both the []string
// and []interface{} metadata encodings (JSON round-trips produce the latter).
func DocTags(doc *domain.Document) []string {
	if doc == nil || doc.Metadata == nil {
		return nil
	}
	switch v := doc.Metadata["tags"].(type) {
	case []string:
		return v
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
