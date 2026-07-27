package domain

import "context"

// The kernel carries three authorization facts on the context so that the
// intermediate OSS helpers whose signatures must not be churned
// (memory.Manager.Query, WorkspaceStage.PrimeForStep, Agent.FetchContext) do not
// each have to grow a parameter: WHO is asking (principal), WHERE they arrived
// from (surface), and the already-resolved read predicate. The boundary injects
// once; the chokepoints read back. ADR-0034 (D5) / ADR-0085.
//
// All three are seeded by the KERNEL from the authenticated connection or
// session. None is ever read from a request payload or from a daemon's claim
// about itself — a black box asserting its own privilege level is not a security
// boundary (INV-5).

// scopeCtxKey is the private context key under which the resolved read predicate
// is carried. The boundary injects once via WithScope; the Search chokepoint
// reads it back.
type scopeCtxKey struct{}

// WithScope returns a child context carrying the effective read predicate.
func WithScope(ctx context.Context, scope *TagPredicate) context.Context {
	return context.WithValue(ctx, scopeCtxKey{}, scope)
}

// ScopeFromContext returns the read predicate carried by ctx, if any. The boolean
// reports PRESENCE — a present-but-nil predicate is distinct from absence, and
// both are treated fail-closed at the chokepoint.
func ScopeFromContext(ctx context.Context) (*TagPredicate, bool) {
	v := ctx.Value(scopeCtxKey{})
	if v == nil {
		return nil, false
	}
	scope, ok := v.(*TagPredicate)
	return scope, ok
}

// principalCtxKey carries the authenticated principal.
type principalCtxKey struct{}

// WithPrincipal returns a child context carrying the authenticated principal.
func WithPrincipal(ctx context.Context, p PrincipalRef) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, p)
}

// PrincipalFromContext returns the principal carried by ctx. A zero PrincipalRef
// means identity could not be established; the Authorizer — not the call site —
// decides what that means (deny in the plugin, allow in OSS).
func PrincipalFromContext(ctx context.Context) PrincipalRef {
	p, _ := ctx.Value(principalCtxKey{}).(PrincipalRef)
	return p
}

// surfaceCtxKey carries the entry point the request arrived through.
type surfaceCtxKey struct{}

// WithSurface returns a child context carrying the request's surface.
func WithSurface(ctx context.Context, s SurfaceRef) context.Context {
	return context.WithValue(ctx, surfaceCtxKey{}, s)
}

// SurfaceFromContext returns the surface carried by ctx. A zero SurfaceRef means
// the request did not arrive through a declared surface (an in-process kernel
// path); the Authorizer decides how to clamp that.
func SurfaceFromContext(ctx context.Context) SurfaceRef {
	s, _ := ctx.Value(surfaceCtxKey{}).(SurfaceRef)
	return s
}

// sessionIDCtxKey carries the conversation/session ID through to the read
// chokepoint so a per-session caller term can be looked up server-side from the
// session record (never from the forgeable Handoff.Context). ADR-0034 (D13).
type sessionIDCtxKey struct{}

// WithSessionID returns a child context carrying the session ID.
func WithSessionID(ctx context.Context, sessionID SessionID) context.Context {
	return context.WithValue(ctx, sessionIDCtxKey{}, sessionID)
}

// SessionIDFromContext returns the session ID carried by ctx, if any.
func SessionIDFromContext(ctx context.Context) (SessionID, bool) {
	v, ok := ctx.Value(sessionIDCtxKey{}).(SessionID)
	return v, ok && v != ""
}
