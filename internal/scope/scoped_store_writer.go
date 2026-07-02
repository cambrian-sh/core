package scope

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// ErrUnknownClassification is returned when an agent's narrow-only hint contains a
// tag outside the controlled vocabulary (coinage). The RPC boundary maps it to
// InvalidArgument. Under ADR-0035 C2 there is no "forbidden classification" write
// error — an agent can only narrow within its operator-set DefaultWriteTags, so it
// can never request a broadening/forbidden classification to be denied.
var ErrUnknownClassification = errors.New("scope: tag outside controlled vocabulary (coinage rejected)")

// WriterScope carries the authenticated identity and operator-configured write
// classification of the principal performing a write (ADR-0035 C2). It is seeded at
// the write boundary (RPC handler or in-process system writer). The kernel DERIVES
// the write's classification from DefaultWriteTags (an agent cannot choose its own);
// the agent may only narrow within it. Its absence means enforcement is not
// configured for this write (kernel curation writes pass through unchanged).
type WriterScope struct {
	WriterID         string
	DefaultWriteTags []string // operator-configured classification ceiling
}

type writerScopeKey struct{}

// WithWriterScope seeds the writer identity and effective scope for downstream
// ScopedStoreWriter validation.
func WithWriterScope(ctx context.Context, ws WriterScope) context.Context {
	return context.WithValue(ctx, writerScopeKey{}, ws)
}

// WriterScopeFromContext returns the writer scope seeded by WithWriterScope.
func WriterScopeFromContext(ctx context.Context) (WriterScope, bool) {
	ws, ok := ctx.Value(writerScopeKey{}).(WriterScope)
	return ws, ok
}

// ScopedStoreWriter is the write-side twin of ScopedVectorStore (ADR-0034 D8/R3,
// amended by ADR-0035). EVERY write — RPC and in-process, including the LLM-driven
// ConsolidatorAgent — passes through it; no principal holds a raw store reference.
// There is no "trusted in-process" carve-out: process membership does not constrain
// a model's output, so validation runs on every path.
//
// On a write that carries a WriterScope (WithWriterScope), it (ADR-0035 C2):
//  1. DERIVES the classification from the writer's operator-configured
//     DefaultWriteTags, narrowed only by the agent's narrow-only hint (the hint can
//     remove tags, never add — an agent can never broaden its own write);
//  2. rejects any hint tag outside the controlled vocabulary (ErrUnknownClassification);
//  3. replaces the agent-supplied tags with the derived classification and
//     kernel-stamps a provenance:source=<writerID> tag — never copied from input.
//
// There is no "forbidden classification" write error: because the agent can only
// narrow within its operator-set DefaultWriteTags, it can never request a
// broadening/forbidden tag to be denied. Reads and other methods pass through the
// embedded store unchanged.
type ScopedStoreWriter struct {
	domain.VectorStore
	vocab  *Vocabulary
	logger *slog.Logger
}

// NewScopedStoreWriter wraps inner with write-side scope enforcement.
func NewScopedStoreWriter(inner domain.VectorStore, vocab *Vocabulary, logger *slog.Logger) *ScopedStoreWriter {
	if logger == nil {
		logger = slog.Default()
	}
	return &ScopedStoreWriter{VectorStore: inner, vocab: vocab, logger: logger}
}

// Save validates and stamps a single document, then delegates.
func (w *ScopedStoreWriter) Save(ctx context.Context, doc *domain.Document) error {
	if err := w.validateAndStamp(ctx, doc); err != nil {
		return err
	}
	return w.VectorStore.Save(ctx, doc)
}

// SaveBatch validates and stamps every document, then delegates. A single
// rejection fails the whole batch (fail-closed).
func (w *ScopedStoreWriter) SaveBatch(ctx context.Context, docs []*domain.Document) error {
	for _, doc := range docs {
		if err := w.validateAndStamp(ctx, doc); err != nil {
			return err
		}
	}
	return w.VectorStore.SaveBatch(ctx, docs)
}

func (w *ScopedStoreWriter) validateAndStamp(ctx context.Context, doc *domain.Document) error {
	ws, ok := WriterScopeFromContext(ctx)
	if !ok {
		return nil // enforcement not configured for this write (kernel curation)
	}
	// ADR-0035 C2: the agent-supplied tags on the doc are a NARROW-ONLY hint; the
	// kernel derives the authoritative classification from DefaultWriteTags.
	final, err := DeriveWriteTags(ws.DefaultWriteTags, tagStrings(doc), w.vocab)
	if err != nil {
		w.logger.WarnContext(ctx, "scope: write denied",
			slog.String("event", "scope_write_deny"),
			slog.String("writer_id", ws.WriterID),
			slog.Any("err", err))
		return err
	}
	// ADR-0035 D8′-4 observability: a narrow-only hint that shares no tag with a
	// non-empty DefaultWriteTags collapses the write to UNCLASSIFIED (matched only by
	// an unrestricted/system reader). This fails safe on confidentiality, but is a
	// silent visibility surprise for the author — surface it so the collapse is
	// diagnosable rather than mysterious "nothing can read my write".
	if len(ws.DefaultWriteTags) > 0 && len(final) == 0 {
		w.logger.WarnContext(ctx, "scope: write narrowed to unclassified",
			slog.String("event", "scope_write_unclassified"),
			slog.String("writer_id", ws.WriterID),
			slog.Any("default_write_tags", ws.DefaultWriteTags),
			slog.Any("hint", tagStrings(doc)))
	}
	if doc.Metadata == nil {
		doc.Metadata = make(map[string]interface{})
	}
	// Replace agent tags with the derived classification, then kernel-stamp provenance.
	doc.Metadata["tags"] = StampSourceProvenance(final, ws.WriterID)
	return nil
}

// DeriveWriteTags computes the kernel-derived classification for a write (ADR-0035
// C2). Classification is the operator-configured DefaultWriteTags, optionally
// narrowed by an agent hint: the hint can only REMOVE tags (intersection), never
// add — so an agent can never make its output more visible than the operator's
// ceiling. A hint tag outside the controlled vocabulary is rejected as coinage
// (ErrUnknownClassification). Provenance tags are exempt and stamped separately.
//
//	final = DefaultWriteTags                     if hint is empty (no narrowing)
//	final = DefaultWriteTags ∩ hint              otherwise (hint-only-removes)
func DeriveWriteTags(defaultWriteTags, narrowHint []string, vocab *Vocabulary) ([]string, error) {
	var hint []string
	for _, t := range narrowHint {
		if IsProvenance(t) {
			continue // never agent-set; ignored
		}
		if !vocab.IsEmpty() && !vocab.Contains(t) {
			return nil, ErrUnknownClassification // coinage rejected
		}
		hint = append(hint, t)
	}
	if len(hint) == 0 {
		return append([]string{}, defaultWriteTags...), nil
	}
	hintSet := make(map[string]struct{}, len(hint))
	for _, t := range hint {
		hintSet[t] = struct{}{}
	}
	out := make([]string, 0, len(defaultWriteTags))
	for _, t := range defaultWriteTags {
		if _, ok := hintSet[t]; ok { // keep only DefaultWriteTags the hint retained
			out = append(out, t)
		}
	}
	return out, nil
}

// StampSourceProvenance returns tags with a kernel-stamped provenance:source=<writerID>,
// stripping any agent-supplied source-provenance tag (forgery defense). An empty
// writerID is a no-op.
func StampSourceProvenance(tags []string, writerID string) []string {
	if writerID == "" {
		return tags
	}
	const sourceKind = provenancePrefix + "source="
	prov := sourceKind + writerID
	out := make([]string, 0, len(tags)+1)
	for _, t := range tags {
		if strings.HasPrefix(t, sourceKind) {
			continue
		}
		out = append(out, t)
	}
	return append(out, prov)
}

// tagStrings extracts the document's classification tags, handling both []string
// and []interface{} metadata encodings.
func tagStrings(doc *domain.Document) []string {
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
