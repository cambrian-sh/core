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
// The decorator does not itself filter rows — it sets opts.Scope and delegates;
// the underlying store (pgvector adapter, or a fake) applies the predicate via
// TagPredicate.Allows. Non-Search methods pass through unchanged (Search is the
// single SQL-building chokepoint).
type EnforcingVectorStore struct {
	domain.VectorStore // embedded: GetByID/GetBatch/Save/... pass through
	logger             *slog.Logger
}

// NewEnforcingVectorStore wraps inner with read enforcement. A nil logger falls
// back to slog.Default().
func NewEnforcingVectorStore(inner domain.VectorStore, logger *slog.Logger) *EnforcingVectorStore {
	if logger == nil {
		logger = slog.Default()
	}
	return &EnforcingVectorStore{VectorStore: inner, logger: logger}
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
		return s.VectorStore.Search(ctx, vector, opts)
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
	return s.VectorStore.Search(ctx, vector, opts)
}
