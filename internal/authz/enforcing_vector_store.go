// Package authz holds the kernel's ENFORCEMENT POINTS (PEP) for access control:
// the read chokepoint, the write chokepoint, and the helpers that ask the
// decision point before the kernel acts.
//
// It contains no policy. Every question here is delegated to a domain.Authorizer,
// which in OSS is the allow-all default and in a premium deployment is the policy
// plugin. The enforcement points are deliberately NOT pluggable: if they were, a
// missing plugin would mean unguarded resource access — the exact failure the
// design exists to prevent (ADR-0085 §4.1).
package authz

import (
	"context"
	"errors"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
)

// ErrScopeMissing is returned by the read chokepoint when a Search reaches it
// without any predicate at all. This converts a dropped-predicate bug (e.g. a
// retrieval started under a fresh context.Background()) into a LOUD failure
// instead of a silent leak.
//
// It is NOT the fail-closed policy: in OSS the Authorizer hands out a bypass
// predicate, so a correctly-wired unscoped deployment never sees this. Only a
// wiring bug does.
var ErrScopeMissing = errors.New("authz: refusing unfiltered Search (fail-closed); seed domain.WithScope or pass SearchOptions.Scope")

// EnforcingVectorStore decorates a domain.VectorStore and applies the resolved
// read predicate on the retrieval path. It is the read-side chokepoint: agents
// are wired with this decorator ONLY, never the base store, so there is no bypass
// path.
//
// Enforcement policy on Search:
//   - no predicate (neither opts nor ctx)   → ErrScopeMissing (fail-closed)
//   - bypass predicate (ScopeSystem or OSS) → delegate unfiltered (audited)
//   - unsatisfiable predicate               → proceed (zero rows) + warn, and the
//     caller's Explain surfaces the reason so the empty result is never silent
//   - otherwise                             → delegate with opts.Scope set
//
// On Search the decorator does not itself filter rows — it sets opts.Scope and
// delegates; the underlying store applies the predicate in SQL. On the BY-IDENTITY
// reads it filters the returned rows directly, because those queries have no
// opts.Scope to push down.
//
// # Why this type does not embed domain.VectorStore
//
// It used to, and the embed was the bug. An embedded interface silently forwards
// every method nobody overrode, so "Search is the single chokepoint" held only while
// every read went through Search — and the compiler could not say otherwise. KG
// expansion reaches chunks BY ID, so it was never covered, and nothing failed: the
// decorator was correctly wired and simply had no opinion about the method being
// called (ADR-0095 D9).
//
// Every method is therefore written out and labelled ENFORCED or PASS-THROUGH. The
// point is not the labels — it is that adding a method to domain.VectorStore now
// breaks this file until someone decides which it is, instead of defaulting to
// unguarded.
type EnforcingVectorStore struct {
	inner  domain.VectorStore
	logger *slog.Logger
}

// NewEnforcingVectorStore wraps inner with read enforcement. A nil logger falls
// back to slog.Default().
func NewEnforcingVectorStore(inner domain.VectorStore, logger *slog.Logger) *EnforcingVectorStore {
	if logger == nil {
		logger = slog.Default()
	}
	return &EnforcingVectorStore{inner: inner, logger: logger}
}

// readPredicate resolves the ctx-carried predicate for a by-identity read. Unlike
// Search there is no opts.Scope to fall back on, so ctx is the only source.
//
// A missing predicate is refused, exactly as Search refuses it: a by-id read with no
// predicate is the same dropped-predicate bug, and in a correctly-wired kernel it is
// unreachable because the OSS Authorizer hands out a bypass. Kernel-internal readers
// that legitimately have no principal seed the explicit, greppable
// domain.WithScope(ctx, &domain.TagPredicate{Bypass: true}).
func (s *EnforcingVectorStore) readPredicate(ctx context.Context, op string) (*domain.TagPredicate, error) {
	eff, ok := domain.ScopeFromContext(ctx)
	if !ok || eff == nil {
		s.logger.WarnContext(ctx, "authz: denied unfiltered by-id read (fail-closed)",
			slog.String("event", "authz_deny"),
			slog.String("reason", string(domain.ReasonNoPrincipal)),
			slog.String("op", op))
		return nil, ErrScopeMissing
	}
	return eff, nil
}

// GetByID — ENFORCED. A document the caller may not read is reported as absent
// rather than as a denial: an unreadable row and a missing row are indistinguishable
// to the caller by design, and every caller already handles nil.
func (s *EnforcingVectorStore) GetByID(ctx context.Context, id string) (*domain.Document, error) {
	eff, err := s.readPredicate(ctx, "GetByID")
	if err != nil {
		return nil, err
	}
	doc, err := s.inner.GetByID(ctx, id)
	if err != nil || doc == nil {
		return doc, err
	}
	if !eff.Bypass && !eff.Allows(DocTags(doc)) {
		s.logger.DebugContext(ctx, "authz: filtered by-id read",
			slog.String("event", "authz_filtered"), slog.String("op", "GetByID"))
		return nil, nil
	}
	return doc, nil
}

// GetBatch — ENFORCED. Survivors only; the caller cannot tell a filtered id from an
// absent one, which is the same information-hiding property as GetByID.
func (s *EnforcingVectorStore) GetBatch(ctx context.Context, ids []string) ([]domain.Document, error) {
	eff, err := s.readPredicate(ctx, "GetBatch")
	if err != nil {
		return nil, err
	}
	docs, err := s.inner.GetBatch(ctx, ids)
	if err != nil || eff.Bypass {
		return docs, err
	}
	return s.filter(ctx, docs, "GetBatch"), nil
}

// ChunksByIDs — ENFORCED batched by-id chunk read (review Q8: the expansion
// lanes' per-id GetByID loops probed four tables each; their ids come from
// chunk_triplets.chunk_id, provably chunks-table ids, so one batched
// chunks-only query replaces up to ~160 round-trips per hop). Optional
// capability: forwarded when the inner store implements it, else served via
// the (4-table) GetBatch so callers can assert on THIS wrapper either way.
// Rows the ctx predicate refuses are dropped, exactly as GetBatch drops them.
func (s *EnforcingVectorStore) ChunksByIDs(ctx context.Context, ids []string) ([]domain.Document, error) {
	eff, err := s.readPredicate(ctx, "ChunksByIDs")
	if err != nil {
		return nil, err
	}
	var docs []domain.Document
	if bf, ok := s.inner.(interface {
		ChunksByIDs(context.Context, []string) ([]domain.Document, error)
	}); ok {
		docs, err = bf.ChunksByIDs(ctx, ids)
	} else {
		docs, err = s.inner.GetBatch(ctx, ids)
	}
	if err != nil || eff.Bypass {
		return docs, err
	}
	return s.filter(ctx, docs, "ChunksByIDs"), nil
}

// QueryByMetadata — ENFORCED. This is the primitive the experiential lane reads
// through (precedent.go resolves an action path by plan_id), so leaving it unguarded
// would mean ADR-0095's classification bound to nothing on that path.
func (s *EnforcingVectorStore) QueryByMetadata(ctx context.Context, filter map[string]string, limit int) ([]domain.Document, error) {
	eff, err := s.readPredicate(ctx, "QueryByMetadata")
	if err != nil {
		return nil, err
	}
	docs, err := s.inner.QueryByMetadata(ctx, filter, limit)
	if err != nil || eff.Bypass {
		return docs, err
	}
	return s.filter(ctx, docs, "QueryByMetadata"), nil
}

// filter drops documents the predicate disallows, preserving order.
func (s *EnforcingVectorStore) filter(ctx context.Context, docs []domain.Document, op string) []domain.Document {
	eff, ok := domain.ScopeFromContext(ctx)
	if !ok || eff == nil {
		return nil // unreachable: callers resolved the predicate already
	}
	out := make([]domain.Document, 0, len(docs))
	for i := range docs {
		if eff.Allows(DocTags(&docs[i])) {
			out = append(out, docs[i])
		}
	}
	if dropped := len(docs) - len(out); dropped > 0 {
		s.logger.DebugContext(ctx, "authz: filtered rows from by-id read",
			slog.String("event", "authz_filtered"), slog.String("op", op), slog.Int("dropped", dropped))
	}
	return out
}

// GetStaleMemories — PASS-THROUGH. Kernel maintenance only: the decay/consolidation
// worker sweeps by activation age, not on behalf of a principal, and the rows never
// reach a caller's context. A principal-facing caller of this method would need it
// enforced; there is none, and adding one is the trigger to revisit.
func (s *EnforcingVectorStore) GetStaleMemories(ctx context.Context, limit int) ([]domain.Document, error) {
	return s.inner.GetStaleMemories(ctx, limit)
}

// The write path — PASS-THROUGH, deliberately. Writes are governed by the OTHER
// chokepoint (EnforcingStoreWriter, which stamps kernel-derived classification per
// ADR-0035). Enforcing writes here as well would put one decision in two places, and
// the second copy is the one nobody reads.

func (s *EnforcingVectorStore) Save(ctx context.Context, doc *domain.Document) error {
	return s.inner.Save(ctx, doc)
}

func (s *EnforcingVectorStore) SaveBatch(ctx context.Context, docs []*domain.Document) error {
	return s.inner.SaveBatch(ctx, docs)
}

func (s *EnforcingVectorStore) Delete(ctx context.Context, id string) error {
	return s.inner.Delete(ctx, id)
}

func (s *EnforcingVectorStore) DeleteBatch(ctx context.Context, ids []string) error {
	return s.inner.DeleteBatch(ctx, ids)
}

func (s *EnforcingVectorStore) IncrementAccess(ctx context.Context, id string) error {
	return s.inner.IncrementAccess(ctx, id)
}

// Search enforces the fail-closed gate, then delegates. It resolves the predicate
// with precedence: an explicit opts.Scope wins; otherwise the ctx-carried one
// (domain.WithScope).
func (s *EnforcingVectorStore) Search(ctx context.Context, vector []float32, opts domain.SearchOptions) ([]domain.SearchResult, error) {
	eff := opts.Scope
	if eff == nil {
		if ctxScope, ok := domain.ScopeFromContext(ctx); ok {
			eff = ctxScope
		}
	}

	// Fail-closed: a Search with no predicate at all is refused, never run
	// unfiltered. In a correctly-wired kernel this is unreachable — the OSS
	// Authorizer supplies a bypass predicate — so reaching it is a bug report.
	if eff == nil {
		s.logger.WarnContext(ctx, "authz: denied unfiltered Search (fail-closed)",
			slog.String("event", "authz_deny"),
			slog.String("reason", string(domain.ReasonNoPrincipal)),
			slog.String("document_type", opts.DocumentType))
		return nil, ErrScopeMissing
	}

	// Explicit, greppable bypass for kernel-internal reads and unscoped OSS.
	if eff.Bypass {
		opts.Scope = eff
		return s.inner.Search(ctx, vector, opts)
	}

	// An unsatisfiable predicate is a SAFE state (zero rows) but otherwise a
	// silent "why is this principal blind?" black box — surface it (INV-3).
	if bad, reason := eff.Unsatisfiable(); bad {
		s.logger.WarnContext(ctx, "authz: unsatisfiable effective predicate",
			slog.String("event", "authz_unsatisfiable"),
			slog.String("reason", reason),
			slog.String("document_type", opts.DocumentType))
	}

	opts.Scope = eff
	return s.inner.Search(ctx, vector, opts)
}
