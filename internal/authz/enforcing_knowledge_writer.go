package authz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
)

// EnforcingKnowledgeWriter is the WRITE chokepoint of the knowledge substrate for the
// kernel's own producers — today the ADR-0136 A1 fact extractor. Constructed ONLY in
// kernel.NewMemoryStack over the raw store, and handed to the extractor as a
// constructor argument (AGENTS.md §2.1, §2.3).
//
// Items (PutClassifiedItem, PutItem) — ENFORCED: the item's classification is a HINT
// (the fact extractor copies its source chunk's tags) and the decision point's
// ClassifyWrite answer replaces it, exactly as EnforcingStoreWriter does for chunks and
// the retired EnforcingFactStore did for memory_facts. A denied item is not written
// (ErrWriteDenied). For memory_fact, the classification-derived identity — the resolution
// key's scope component and the subject reference — is then recomputed from the FINAL
// tags (domain.RekeyFactItem), so supersession only ever relates items the decision
// point classified alike, never items whose hints merely agreed.
//
// The rest — assessments, predicate profiles, item embeddings, extraction markers — is
// PASS-THROUGH, each labelled: they are kernel-authored rows over items that already
// passed this chokepoint (an assessment's classification is its inputs'; an embedding is
// a vector of a stored item; a marker is a document id and a digest).
//
// Like EnforcingVectorStore it does not embed its inner port: every method is written
// out, so adding a method to the port breaks this file until someone decides whether it
// is enforced.
type EnforcingKnowledgeWriter struct {
	inner  domain.FactSubstrateStore
	authz  domain.Authorizer
	logger *slog.Logger
}

var (
	_ domain.FactSubstrateWriter = (*EnforcingKnowledgeWriter)(nil)
)

// NewEnforcingKnowledgeWriter wraps inner. A nil authorizer is the OSS allow-all
// default; a nil logger is slog.Default().
func NewEnforcingKnowledgeWriter(inner domain.FactSubstrateStore, a domain.Authorizer, logger *slog.Logger) *EnforcingKnowledgeWriter {
	if a == nil {
		a = domain.AllowAllAuthorizer{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &EnforcingKnowledgeWriter{inner: inner, authz: a, logger: logger}
}

// PutClassifiedItem — ENFORCED (classified write). Returns the item as stored.
func (w *EnforcingKnowledgeWriter) PutClassifiedItem(ctx context.Context, item domain.KnowledgeItem) (domain.KnowledgeItem, bool, error) {
	principal := domain.PrincipalFromContext(ctx)
	final, dec := w.authz.ClassifyWrite(ctx, principal, domain.ClassificationHint(item.Classification))
	if !dec.Allowed {
		w.logger.WarnContext(ctx, "authz: knowledge item write denied",
			slog.String("event", "authz_write_deny"),
			slog.String("principal", principal.String()),
			slog.String("reason", string(dec.Reason)),
			slog.String("kind", item.Kind))
		return domain.KnowledgeItem{}, false, fmt.Errorf("knowledge item %s: %w", item.ID, ErrWriteDenied)
	}
	// ADR-0142 D12: private to the conversation owner the work is for.
	item.Classification = domain.StampPrivateOwner(ctx, append([]string(nil), final...))
	item = domain.RekeyFactItem(item)
	id, inserted, err := w.inner.PutItem(ctx, item)
	if err != nil {
		return domain.KnowledgeItem{}, false, err
	}
	if !inserted {
		// A replay: the item on record is the one later reads key on (its assertion
		// time, its classification), not the re-submitted copy.
		if got, gerr := w.inner.GetItem(ctx, id); gerr == nil && got != nil {
			return *got, false, nil
		}
	}
	item.ID = id
	return item, inserted, nil
}

// PutItem — ENFORCED: PutClassifiedItem, with the domain.KnowledgeStore shape.
func (w *EnforcingKnowledgeWriter) PutItem(ctx context.Context, item domain.KnowledgeItem) (domain.KnowledgeItemID, bool, error) {
	stored, inserted, err := w.PutClassifiedItem(ctx, item)
	if err != nil {
		return "", false, err
	}
	return stored.ID, inserted, nil
}

// GetAssessment — PASS-THROUGH. Its one kernel caller is the fact extractor looking up
// the keyed id of a candidate set it built from items it read under the inputs' own
// classification; the id cannot name another classification's assessment.
func (w *EnforcingKnowledgeWriter) GetAssessment(ctx context.Context, id string) (*domain.Assessment, error) {
	return w.inner.GetAssessment(ctx, id)
}

// PutAssessment — PASS-THROUGH (kernel-authored; classification = its inputs').
func (w *EnforcingKnowledgeWriter) PutAssessment(ctx context.Context, a domain.Assessment) (bool, error) {
	return w.inner.PutAssessment(ctx, a)
}

// PredicateProfile — PASS-THROUGH (namespace-wide counts; provenance ids resolve only
// under a reader's scope, A1.4).
func (w *EnforcingKnowledgeWriter) PredicateProfile(ctx context.Context, namespace, kind, predicate string) (domain.PredicateProfile, error) {
	return w.inner.PredicateProfile(ctx, namespace, kind, predicate)
}

// VoteProfile — PASS-THROUGH (kernel-authored counts).
func (w *EnforcingKnowledgeWriter) VoteProfile(ctx context.Context, namespace, kind, predicate string, v domain.ProfileVote, minVotes int) (bool, error) {
	return w.inner.VoteProfile(ctx, namespace, kind, predicate, v, minVotes)
}

// SeedDeclaredProfiles — PASS-THROUGH (operator config).
func (w *EnforcingKnowledgeWriter) SeedDeclaredProfiles(ctx context.Context, namespace, kind string, declared map[string]string, minVotes int, setBy string) ([]string, error) {
	return w.inner.SeedDeclaredProfiles(ctx, namespace, kind, declared, minVotes, setBy)
}

// RederivePredicate — PASS-THROUGH (re-derivation of stored rows).
func (w *EnforcingKnowledgeWriter) RederivePredicate(ctx context.Context, namespace, kind, predicate string, limit int) (int, error) {
	return w.inner.RederivePredicate(ctx, namespace, kind, predicate, limit)
}

// PutItemEmbeddings — PASS-THROUGH (vectors of stored items).
func (w *EnforcingKnowledgeWriter) PutItemEmbeddings(ctx context.Context, embs []domain.ItemEmbedding) error {
	return w.inner.PutItemEmbeddings(ctx, embs)
}

// ExtractionMarker — PASS-THROUGH (document id + digest + version/coverage bookkeeping;
// no content).
func (w *EnforcingKnowledgeWriter) ExtractionMarker(ctx context.Context, namespace, extractor, documentID, contentHash string) (*domain.ExtractionMarker, error) {
	return w.inner.ExtractionMarker(ctx, namespace, extractor, documentID, contentHash)
}

// MarkExtracted — PASS-THROUGH (same bookkeeping row).
func (w *EnforcingKnowledgeWriter) MarkExtracted(ctx context.Context, m domain.ExtractionMarker) error {
	return w.inner.MarkExtracted(ctx, m)
}

// PutExtractionWindow — PASS-THROUGH (the two-phase backfill's journal: a document id,
// a digest, a passage range and the ids of items that already passed PutClassifiedItem;
// no content).
func (w *EnforcingKnowledgeWriter) PutExtractionWindow(ctx context.Context, win domain.ExtractionWindow) error {
	return w.inner.PutExtractionWindow(ctx, win)
}

// ExtractionWindows — PASS-THROUGH (same bookkeeping rows; the items they name are read
// back through the enforcing fact reader).
func (w *EnforcingKnowledgeWriter) ExtractionWindows(ctx context.Context, namespace, extractor, documentID, contentHash, version string) ([]domain.ExtractionWindow, error) {
	return w.inner.ExtractionWindows(ctx, namespace, extractor, documentID, contentHash, version)
}

// DeleteExtractionWindows — PASS-THROUGH (same bookkeeping rows).
func (w *EnforcingKnowledgeWriter) DeleteExtractionWindows(ctx context.Context, namespace, extractor, documentID, contentHash string) error {
	return w.inner.DeleteExtractionWindows(ctx, namespace, extractor, documentID, contentHash)
}

// EnforcingFactItemReader is the READ chokepoint of the fact layer over the substrate
// (A1.5; the read half of the retired EnforcingFactStore, moved). Every read resolves
// its predicate from the argument/options, else from ctx; none at all is
// ErrScopeMissing (fail closed). The adapter applies it IN SQL to the item
// classification.
//
// A fact read is never the last word on visibility: the fact lane fetches each hit's
// SOURCE CHUNK through EnforcingVectorStore.GetBatch with the predicate on ctx, and a
// fact whose chunk the reader cannot see contributes nothing (TestFacts_ScopeOnByIDRead).
type EnforcingFactItemReader struct {
	inner  domain.FactItemReader
	logger *slog.Logger
}

var _ domain.FactItemReader = (*EnforcingFactItemReader)(nil)

// NewEnforcingFactItemReader wraps inner.
func NewEnforcingFactItemReader(inner domain.FactItemReader, logger *slog.Logger) *EnforcingFactItemReader {
	if logger == nil {
		logger = slog.Default()
	}
	return &EnforcingFactItemReader{inner: inner, logger: logger}
}

func (r *EnforcingFactItemReader) resolve(ctx context.Context, explicit *domain.TagPredicate, op string) (*domain.TagPredicate, error) {
	eff := explicit
	if eff == nil {
		if c, ok := domain.ScopeFromContext(ctx); ok {
			eff = c
		}
	}
	if eff == nil {
		r.logger.WarnContext(ctx, "authz: denied unfiltered fact read (fail-closed)",
			slog.String("event", "authz_deny"),
			slog.String("reason", string(domain.ReasonNoPrincipal)),
			slog.String("op", op))
		return nil, ErrScopeMissing
	}
	return eff, nil
}

// SearchFacts — ENFORCED.
func (r *EnforcingFactItemReader) SearchFacts(ctx context.Context, vec []float32, opts domain.FactSearchOptions) ([]domain.FactHit, error) {
	eff, err := r.resolve(ctx, opts.Scope, "SearchFacts")
	if err != nil {
		return nil, err
	}
	opts.Scope = eff
	return r.inner.SearchFacts(ctx, vec, opts)
}

// FactChain — ENFORCED.
func (r *EnforcingFactItemReader) FactChain(ctx context.Context, f domain.Fact, scope *domain.TagPredicate) ([]domain.Fact, error) {
	eff, err := r.resolve(ctx, scope, "FactChain")
	if err != nil {
		return nil, err
	}
	return r.inner.FactChain(ctx, f, eff)
}

// SimilarFacts — ENFORCED. Its one caller is the extractor's assessment pass, which
// reads with the explicit ScopeSystem bypass but within the new fact's own subject
// reference (scope key + subject), so the candidates are of exactly its classification.
func (r *EnforcingFactItemReader) SimilarFacts(ctx context.Context, f domain.Fact, vec []float32, k int, scope *domain.TagPredicate) ([]domain.Fact, error) {
	eff, err := r.resolve(ctx, scope, "SimilarFacts")
	if err != nil {
		return nil, err
	}
	return r.inner.SimilarFacts(ctx, f, vec, k, eff)
}

var _ domain.FactStagedReader = (*EnforcingFactItemReader)(nil)

// ErrStagedReadsUnsupported is returned by the staged reads when the inner reader does
// not implement domain.FactStagedReader.
var ErrStagedReadsUnsupported = errors.New("authz: the fact reader has no staged reads (domain.FactStagedReader)")

// HasStagedReads reports whether the inner reader implements domain.FactStagedReader —
// the two-phase backfill checks it before it relies on the staged reads.
func (r *EnforcingFactItemReader) HasStagedReads() bool {
	_, ok := r.inner.(domain.FactStagedReader)
	return ok
}

// FactsByID — ENFORCED (a by-id read: the predicate is applied IN SQL, AGENTS.md §2.4).
func (r *EnforcingFactItemReader) FactsByID(ctx context.Context, ids []domain.KnowledgeItemID, scope *domain.TagPredicate) ([]domain.Fact, error) {
	eff, err := r.resolve(ctx, scope, "FactsByID")
	if err != nil {
		return nil, err
	}
	sr, ok := r.inner.(domain.FactStagedReader)
	if !ok {
		return nil, ErrStagedReadsUnsupported
	}
	return sr.FactsByID(ctx, ids, eff)
}

// SimilarFactsHiding — ENFORCED, like SimilarFacts.
func (r *EnforcingFactItemReader) SimilarFactsHiding(ctx context.Context, f domain.Fact, vec []float32, k int, scope *domain.TagPredicate, hidden []domain.KnowledgeItemID) ([]domain.Fact, error) {
	eff, err := r.resolve(ctx, scope, "SimilarFactsHiding")
	if err != nil {
		return nil, err
	}
	sr, ok := r.inner.(domain.FactStagedReader)
	if !ok {
		return nil, ErrStagedReadsUnsupported
	}
	return sr.SimilarFactsHiding(ctx, f, vec, k, eff, hidden)
}

// ErrReservedKind is returned by ReservedKindGuard for a kernel-reserved kind.
var ErrReservedKind = errors.New("authz: kernel-reserved knowledge kind")

// ReservedKindGuard is the plugin seam's knowledge store (KernelServices.Knowledge):
// the raw store with the kernel-reserved kinds fenced off. memory_fact items are
// principal-classified content written only through EnforcingKnowledgeWriter; the raw
// seam stores a caller's classification verbatim and reads with no predicate
// (T-20261103-kernelservices-knowledge-hands-plugins-the-raw), so a plugin writing,
// reading or erasing memory_fact through it would bypass both chokepoints. Plugins read
// memory_fact resolutions through the SCOPED query plane instead (A1-Q6). Other kinds
// pass through unchanged — closing the general seam gap is that ticket's work.
type ReservedKindGuard struct {
	inner    domain.KnowledgeStore
	reserved map[string]bool
}

var _ domain.KnowledgeStore = (*ReservedKindGuard)(nil)

// NewReservedKindGuard fences kinds off inner.
func NewReservedKindGuard(inner domain.KnowledgeStore, kinds ...string) *ReservedKindGuard {
	r := map[string]bool{}
	for _, k := range kinds {
		r[k] = true
	}
	return &ReservedKindGuard{inner: inner, reserved: r}
}

func (g *ReservedKindGuard) refuse(kind, op string) error {
	return fmt.Errorf("knowledge %s: kind %q: %w", op, kind, ErrReservedKind)
}

// PutItem — refused for a reserved kind.
func (g *ReservedKindGuard) PutItem(ctx context.Context, item domain.KnowledgeItem) (domain.KnowledgeItemID, bool, error) {
	if g.reserved[item.Kind] {
		return "", false, g.refuse(item.Kind, "put")
	}
	return g.inner.PutItem(ctx, item)
}

// GetItem — an item of a reserved kind reads as refused.
func (g *ReservedKindGuard) GetItem(ctx context.Context, id domain.KnowledgeItemID) (*domain.KnowledgeItem, error) {
	it, err := g.inner.GetItem(ctx, id)
	if err != nil {
		return nil, err
	}
	if it != nil && g.reserved[it.Kind] {
		return nil, g.refuse(it.Kind, "get")
	}
	return it, nil
}

// CurrentResolutions — refused for a reserved kind.
func (g *ReservedKindGuard) CurrentResolutions(ctx context.Context, namespace, kind, policy string) ([]domain.Resolution, error) {
	if g.reserved[kind] {
		return nil, g.refuse(kind, "current resolutions")
	}
	return g.inner.CurrentResolutions(ctx, namespace, kind, policy)
}

// EraseItems — refused for a reserved kind (its erasure is ADR-0137's kernel path).
func (g *ReservedKindGuard) EraseItems(ctx context.Context, namespace, kind string, sel domain.KnowledgeErasure) (int, error) {
	if g.reserved[kind] {
		return 0, g.refuse(kind, "erase")
	}
	return g.inner.EraseItems(ctx, namespace, kind, sel)
}
