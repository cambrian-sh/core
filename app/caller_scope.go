package app

import (
	"context"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// Wiring caller_scope at session start (BRAIN-01).
//
// `Session.CallerScope` and `SessionManager.CreateScopedSession` have existed
// since ADR-0034 D13, and **nothing in production ever called them**: every
// session was opened with the unscoped constructor, so every caller_scope was
// empty, the premium authorizer's per-session term contributed nothing, and
// `effective = caller_scope ∩ agent_scope` reduced to the agent's own scope. The
// mechanism was complete and inert.
//
// This is the missing call.
//
// Where the scope comes from matters more than that it exists. It is resolved
// SERVER-SIDE from the authenticated operator principal and the surface the
// kernel stamped — never from a request field. A caller restating its own
// entitlements is not a security boundary (INV-5), which is the same reason
// `Session.Surface` is decided once by the kernel.

// callerScopeFor resolves the authenticated caller's scope term for a new
// session.
//
// Returns the zero TagSet — no constraint — in every case where a scope cannot be
// established. That is deliberate and is NOT a fail-open hole: caller_scope is a
// NARROWING term that is intersected with the agent's own scope, so an empty one
// leaves enforcement exactly where it is today (the agent's scope alone) rather
// than granting anything. Refusing to open the session instead would take down
// every unscoped deployment to protect a term that adds nothing there.
//
// What it must never do is invent a NARROWER-looking scope than the caller
// actually has, or a WIDER one than the decision point granted. Both are covered
// below.
func callerScopeFor(ctx context.Context, authz domain.Authorizer) domain.TagSet {
	if authz == nil {
		return domain.TagSet{}
	}
	principal, _, ok := operator.PrincipalFromContext(ctx)
	if !ok || principal == "" {
		// No authenticated operator: a kernel-internal or test path. Nothing to
		// scope to, and inventing an identity here would be worse than none.
		return domain.TagSet{}
	}

	return callerScopeForPrincipal(ctx, authz, principal, domain.SurfaceFromContext(ctx))
}

// callerScopeForPrincipal is the decision, split from the context plumbing so it
// is testable without exporting a way to forge an operator principal. The only
// thing allowed to establish that principal is the auth interceptor.
func callerScopeForPrincipal(
	ctx context.Context,
	authz domain.Authorizer,
	principal string,
	surface domain.SurfaceRef,
) domain.TagSet {
	ref := domain.PrincipalRef{ID: principal, Kind: domain.PrincipalUser}
	pred, dec := authz.ReadFilter(ctx, ref, surface)

	// A nil predicate means the decision point authorizes NO read for this
	// principal. Storing an empty (unconstrained) caller_scope would invert that,
	// so the session is opened without one and the denial is logged — the agent's
	// own scope still governs, and the operator sees why their reads come back
	// empty rather than silently getting everything.
	if pred == nil {
		slog.WarnContext(ctx, "BRAIN-01: no read authorized for the session opener; "+
			"opening without a caller_scope",
			slog.String("principal", principal),
			slog.String("reason", string(dec.Reason)))
		return domain.TagSet{}
	}

	set, representable := domain.TagSetFromPredicate(pred)
	if !representable {
		// Conjunctive normal form with more than one clause. Flattening it into a
		// single OR-set would WIDEN the caller's scope — see TagSetFromPredicate.
		// Dropping it leaves the agent's scope in charge, which is narrower than
		// widening and is the safe direction.
		slog.WarnContext(ctx, "BRAIN-01: caller predicate is not representable as a "+
			"caller_scope term (multi-clause CNF); opening without one rather than widening it",
			slog.String("principal", principal))
		return domain.TagSet{}
	}
	return set
}
