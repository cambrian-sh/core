package authz

import (
	"context"
	"errors"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
)

// ErrWriteDenied is returned when the decision point refuses a write outright
// (e.g. a tag outside the controlled vocabulary) or the write's requested
// classification cannot be honoured (ENG-2115: *UnhonouredClassificationError
// unwraps to it). The RPC boundary maps it to InvalidArgument. The error names at
// most the tags the writer itself sent; the decision point's detail (which may list
// the vocabulary) travels in the AccessDecision, which the chokepoint logs, so the
// error cannot be used to probe the vocabulary.
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
// narrowed (never broadened) by the writer's hint, and stamps provenance. A hint
// the answer does not fully carry — a requested tag outside the writer's write
// ceiling, or a writer with none — refuses the write (ENG-2115,
// ClassifyWriteStrict); it is never stored with the requested tags dropped.
//
// It is a domain.VectorWriter and nothing more (ENG-1476): it has no read methods,
// so it can never become a read path that skips EnforcingVectorStore, and a consumer
// that reads and writes is handed the two tiers composed (the memory stack does
// this), not one type that does both.
//
// Delete and DeleteBatch are KERNEL MAINTENANCE ONLY (ADR-0137 D3, ENG-411 stage B): a
// row-by-id delete leaves the record's derivatives (document edges, fact items) and no
// tombstone or receipt, so it is never a principal's way to forget. The system principal
// with an explicit bypass predicate on ctx (the nightly decay sweep) may delete; no
// principal or no predicate is ErrScopeMissing; any other principal is ErrEraseDenied —
// a principal forgets through the erasure chokepoint (EnforcingEraser).
//
// Every classified write is also stamped with its writer of record
// (domain.MetaWrittenBy = the ctx principal's WriterID), replacing whatever the writer
// put there: it is what an agent's erasure is checked against.
type EnforcingStoreWriter struct {
	inner  domain.VectorWriter
	authz  domain.Authorizer
	logger *slog.Logger
}

var _ domain.VectorWriter = (*EnforcingStoreWriter)(nil)

// NewEnforcingStoreWriter wraps inner with write-side enforcement. A nil
// authorizer falls back to the OSS allow-all default; a nil logger to slog.Default().
func NewEnforcingStoreWriter(inner domain.VectorWriter, a domain.Authorizer, logger *slog.Logger) *EnforcingStoreWriter {
	if a == nil {
		a = domain.AllowAllAuthorizer{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &EnforcingStoreWriter{inner: inner, authz: a, logger: logger}
}

// Save classifies and stamps a single document, then delegates.
func (w *EnforcingStoreWriter) Save(ctx context.Context, doc *domain.Document) error {
	if err := w.classify(ctx, doc); err != nil {
		return err
	}
	return w.inner.Save(ctx, doc)
}

// SaveBatch classifies and stamps every document, then delegates. A single
// rejection fails the whole batch (fail-closed).
func (w *EnforcingStoreWriter) SaveBatch(ctx context.Context, docs []*domain.Document) error {
	for _, doc := range docs {
		if err := w.classify(ctx, doc); err != nil {
			return err
		}
	}
	return w.inner.SaveBatch(ctx, docs)
}

// Delete removes one row by id — kernel maintenance only (see the type comment).
func (w *EnforcingStoreWriter) Delete(ctx context.Context, id string) error {
	if err := w.maintenanceDelete(ctx, 1); err != nil {
		return err
	}
	return w.inner.Delete(ctx, id)
}

// DeleteBatch removes rows by id — kernel maintenance only, as Delete.
func (w *EnforcingStoreWriter) DeleteBatch(ctx context.Context, ids []string) error {
	if err := w.maintenanceDelete(ctx, len(ids)); err != nil {
		return err
	}
	return w.inner.DeleteBatch(ctx, ids)
}

// maintenanceDelete admits a row delete only for the kernel acting under an explicit
// bypass predicate (ADR-0137 D3: kernel GC seeds the greppable bypass).
func (w *EnforcingStoreWriter) maintenanceDelete(ctx context.Context, n int) error {
	principal := domain.PrincipalFromContext(ctx)
	pred, present := domain.ScopeFromContext(ctx)
	if principal.IsZero() || !present || pred == nil {
		return ErrScopeMissing
	}
	if principal.Kind == domain.PrincipalSystem && pred.Bypass {
		return nil
	}
	w.logger.WarnContext(ctx, "authz: row delete denied",
		slog.String("event", "authz_delete_deny"),
		slog.String("principal", principal.String()),
		slog.Int("rows", n),
		slog.String("detail", "row deletes are kernel maintenance; a principal forgets through the erasure path"))
	return ErrEraseDenied
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
	//
	// ENG-2115: fail closed — a write that cannot carry every classification tag it
	// requested is refused with an error naming them (ClassifyWriteStrict), never
	// stored without them.
	final, dec, err := ClassifyWriteStrict(ctx, w.authz, principal, domain.ClassificationHint(DocTags(doc)))
	if err != nil {
		w.logger.WarnContext(ctx, "authz: write denied",
			slog.String("event", "authz_write_deny"),
			slog.String("principal", principal.String()),
			slog.String("reason", string(dec.Reason)),
			slog.String("detail", dec.Detail),
			slog.String("error", err.Error()))
		return err
	}
	// A write that ends up with no classification at all is readable only by a
	// reader whose predicate has no positive (required / any-of) terms: an
	// unrestricted, bypass or forbidden-only reader. When StampPrivateOwner below
	// adds owner:<id>, that owner also reads it through the own-rows rule
	// (ADR-0146). A reader holding any positive term cannot see it. That fails
	// safe on confidentiality but is a silent visibility surprise for the author,
	// so it is surfaced (INV-3). The
	// decision point emits the richer version of this warning — it is the only
	// party that can tell a classification tag from a provenance stamp.
	//
	// Not for the kernel's own records (agent profiles, judicial records): they
	// are kernel-authored, unclassified by design, and read by the kernel, so the
	// "author surprise" this warns about does not exist — it only fired on every
	// profile save (ENG-263). A premium decision point stamps them
	// provenance:source=system:kernel; in OSS they stay untagged, silently.
	if len(final) == 0 && principal.Kind != domain.PrincipalSystem {
		w.logger.WarnContext(ctx, "authz: write carries no classification",
			slog.String("event", "authz_write_unclassified"),
			slog.String("principal", principal.String()),
			slog.Any("requested", DocTags(doc)))
	}
	// ADR-0142 D12: a write made while working a person's conversation is
	// private to that person (owner:<id>), whatever the writer's own tags.
	final = domain.StampPrivateOwner(ctx, final)
	if doc.Metadata == nil {
		doc.Metadata = make(map[string]interface{})
	}
	doc.Metadata["tags"] = final
	// ADR-0137 D3: the writer of record is the kernel's to stamp, never the writer's to
	// claim — a write with no principal carries none (operator-erasable only).
	if principal.IsZero() {
		delete(doc.Metadata, domain.MetaWrittenBy)
	} else {
		doc.Metadata[domain.MetaWrittenBy] = principal.WriterID()
	}
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
