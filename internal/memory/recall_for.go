package memory

import (
	"context"

	"github.com/cambrian-sh/core/domain"
)

// embedQuery embeds RECALL text: through EmbedQuery when the embedder
// distinguishes query from document (ADR-0048, e.g. bge-large's "Represent this
// sentence for searching relevant passages: " instruction), else plain Embed
// (symmetric embedders such as nomic). Stored documents are always embedded
// bare; only the query side carries the prefix.
//
// It is the ONE query-embedding helper (ENG-1339 / ENG-465, owner decision
// 2026-11-14): every recall pass (searchByType) and every component that needs
// a query vector of its own (WorkspaceStage's retrieval-session log) embeds
// through it, so no recall path can again embed a query as a document — the
// watcher's and the planner's priming did, which degraded their dense ranking
// against the default asymmetric embedder.
func embedQuery(ctx context.Context, e domain.Embedder, text string) ([]float32, error) {
	if qe, ok := e.(interface {
		EmbedQuery(context.Context, string) ([]float32, error)
	}); ok {
		return qe.EmbedQuery(ctx, text)
	}
	return e.Embed(ctx, text)
}

// readAsCtxKey carries the principal RecallFor reads as. Unexported: only
// RecallFor seeds it, server-side, so no caller-supplied value can reach it.
type readAsCtxKey struct{}

func readAsFrom(ctx context.Context) (domain.PrincipalRef, bool) {
	p, ok := ctx.Value(readAsCtxKey{}).(domain.PrincipalRef)
	return p, ok && p.ID != ""
}

// RecallFor is one single-pass recall of one document type, read AS principal
// p — the principal the work is for (ENG-1339 / ENG-465, owner decision
// 2026-11-14: kernel priming of a signal's or a plan's prompt reads memory as
// that principal through QueryService, never at ScopeSystem around it). It runs
// the same pass an agent's recall runs (searchByType: query-prefixed embedding,
// hybrid/RRF, Stage-A blend, the ADR-0136 fact lane and its current_fact /
// supersession features, the relevance floor, the decision record), with the
// read predicate the Authorizer resolves for p on this request's surface.
//
// A zero principal reads nothing: (nil, nil). There is no fallback to a kernel
// read — "no principal ⇒ no priming" is the decision, and a nil predicate from
// the Authorizer is likewise an empty result (searchByType logs the denial).
//
// Any read scope an upstream caller left on ctx (including ScopeSystem) is
// shadowed, so the read can only ever be p's. Single pass on purpose: priming is
// a prompt section, not an agent's grounding loop, and the agentic loop would
// spend planner calls on every plan or signal.
func (q *QueryService) RecallFor(ctx context.Context, p domain.PrincipalRef, query, docType string) ([]domain.SearchResult, error) {
	if p.ID == "" {
		return nil, nil
	}
	ctx = domain.WithScope(ctx, nil)
	ctx = context.WithValue(ctx, readAsCtxKey{}, p)
	ctx = domain.EnsureQueryCorrelation(ctx, p.String(), query)
	return q.searchByType(ctx, query, p.String(), docType, docType == domain.DocTypeMnemonicFact)
}
