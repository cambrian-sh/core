// Package scope holds the CORE access-control enforcement primitives for
// ADR-0034: the read-side ScopedVectorStore decorator, the write-side
// ScopedStoreWriter decorator, and the controlled-vocabulary gate. These are
// compiled into every build (no premium tag) — a security primitive must not be
// paywalled. Governance (policy authoring, retained audit store) is premium.
package scope

import (
	"context"
	"errors"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
)

// ErrScopeMissing is returned by the read chokepoint when a Search reaches it
// without an effective scope. This converts any dropped-scope bug (e.g. a
// retrieval started under a fresh context.Background()) into a LOUD failure
// instead of a silent leak. ADR-0034 (D5/D6).
var ErrScopeMissing = errors.New("scope: refusing unscoped Search (fail-closed); seed domain.WithScope or pass SearchOptions.Scope")

// ScopedVectorStore decorates a domain.VectorStore and enforces ADR-0034 access
// scoping on the retrieval path. It is the read-side chokepoint: agents are wired
// with this decorator ONLY, never the base store, so there is no bypass path.
//
// Enforcement policy on Search:
//   - no scope (nil, from neither opts nor ctx)  → ErrScopeMissing (fail-closed)
//   - ScopeSystem sentinel                       → bypass filtering (audited)
//   - unsatisfiable effective scope              → proceed (zero rows) + warn log
//   - otherwise                                  → delegate with opts.Scope set
//
// The decorator does not itself filter rows — it sets opts.Scope and delegates;
// the underlying store (pgvector adapter, or a fake) applies the predicate via
// EffectiveScope.Allows. Non-Search methods pass through unchanged (the Search
// path is the single SQL-building chokepoint per D5).
type ScopedVectorStore struct {
	domain.VectorStore // embedded: GetByID/GetBatch/Save/... pass through
	logger             *slog.Logger
}

// NewScopedVectorStore wraps inner with scope enforcement. A nil logger falls
// back to slog.Default().
func NewScopedVectorStore(inner domain.VectorStore, logger *slog.Logger) *ScopedVectorStore {
	if logger == nil {
		logger = slog.Default()
	}
	return &ScopedVectorStore{VectorStore: inner, logger: logger}
}

// Search enforces the fail-closed scope gate, then delegates. It resolves the
// effective scope with precedence: an explicit opts.Scope wins; otherwise the
// ctx-carried scope (domain.WithScope) is used.
func (s *ScopedVectorStore) Search(ctx context.Context, vector []float32, opts domain.SearchOptions) ([]domain.SearchResult, error) {
	eff := opts.Scope
	if eff == nil {
		if ctxScope, ok := domain.ScopeFromContext(ctx); ok {
			eff = ctxScope
		}
	}

	// Fail-closed: a Search with no scope at all is refused, never run unfiltered.
	if eff == nil {
		s.logger.WarnContext(ctx, "scope: denied unscoped Search (fail-closed)",
			slog.String("event", "scope_deny"),
			slog.String("reason", "missing_scope"),
			slog.String("document_type", opts.DocumentType))
		return nil, ErrScopeMissing
	}

	// Explicit, greppable system bypass for kernel-internal reads.
	if eff.System {
		s.logger.InfoContext(ctx, "scope: ScopeSystem read (filtering bypassed)",
			slog.String("event", "scope_system"),
			slog.String("document_type", opts.DocumentType))
		opts.Scope = eff
		return s.VectorStore.Search(ctx, vector, opts)
	}

	// Unsatisfiable effective scope is a SAFE state (zero rows) but otherwise a
	// silent "why is this agent blind?" black box — surface it as a warning.
	if bad, reason := eff.Unsatisfiable(); bad {
		s.logger.WarnContext(ctx, "scope: unsatisfiable effective scope",
			slog.String("event", "scope_unsatisfiable"),
			slog.String("reason", reason),
			slog.String("document_type", opts.DocumentType))
	}

	opts.Scope = eff
	return s.VectorStore.Search(ctx, vector, opts)
}
