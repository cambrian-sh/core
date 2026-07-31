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

// isolationCtxKey carries the session-isolation predicate (BRAIN-01).
//
// Seeded on the SAME context as the read predicate and for the same stated
// reason: Search pushes it into SQL, but the enrichment stages reach chunks BY ID
// (anchor promotion, neighbour window, entity seeding, kgExpand) and ctx is their
// only channel. Seeding once means a by-id read added later is isolated by
// default rather than by whoever remembers to thread it.
//
// It is a SECOND predicate rather than more terms on the first, because the two
// answer different questions — may this principal see this CLASS of thing, versus
// does this belong to the conversation I am answering — and a session id is an
// identity, not a classification (ADR-0099).
type isolationCtxKey struct{}

// WithIsolation returns a child context carrying the session-isolation predicate.
func WithIsolation(ctx context.Context, iso *SessionIsolation) context.Context {
	return context.WithValue(ctx, isolationCtxKey{}, iso)
}

// IsolationFromContext returns the isolation predicate carried by ctx, if any.
// The boolean reports PRESENCE — a present-but-nil predicate is distinct from
// absence, and both are treated fail-closed at the chokepoint.
func IsolationFromContext(ctx context.Context) (*SessionIsolation, bool) {
	v := ctx.Value(isolationCtxKey{})
	if v == nil {
		return nil, false
	}
	iso, ok := v.(*SessionIsolation)
	return iso, ok
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
