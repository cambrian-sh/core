package domain

import "context"

// The kernel carries three authorization facts on the context so that the
// intermediate OSS helpers whose signatures must not be churned
// (memory.Manager.Query, Agent.FetchContext) do not
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

// SurfaceFromContextOr returns the surface carried by ctx, or def when none was
// stamped. For agent-plane call sites that used to hard-code their surface: the
// stamped one is the turn's real entry point (the transport's, or the recorded
// session/conversation surface the ADR-0085 D7 clamp re-stamps), and the
// hard-coded value is kept only for an in-process path that carries none
// (T-20261113-surface-clamp-is-applied-only-on-querymemory).
func SurfaceFromContextOr(ctx context.Context, def SurfaceRef) SurfaceRef {
	if s := SurfaceFromContext(ctx); s.Kind != "" || s.ID != "" {
		return s
	}
	return def
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

// resolvedLeaseCtxKey marks that the kernel RESOLVED the caller's lease and found
// it bound to the calling agent (ADR-0139 D8) — distinct from "no lease
// presented" and from "a lease presented that resolved to nothing" (ENG-518). Set
// only by the network layer from its own lease registry, never from a header
// value as such.
type resolvedLeaseCtxKey struct{}

// WithResolvedLease marks ctx as carrying a lease the kernel resolved.
func WithResolvedLease(ctx context.Context, lease LeaseID) context.Context {
	if lease == "" {
		return ctx
	}
	return context.WithValue(ctx, resolvedLeaseCtxKey{}, lease)
}

// ResolvedLeaseFromContext returns the resolved lease; ok is false when no lease
// was presented or the presented one did not resolve to this caller.
func ResolvedLeaseFromContext(ctx context.Context) (LeaseID, bool) {
	l, ok := ctx.Value(resolvedLeaseCtxKey{}).(LeaseID)
	return l, ok && l != ""
}

// runPrincipalCtxKey carries the principal a unit of work is done FOR — the
// person whose own integration credentials a tool call may use (ADR-0140 D4).
//
// It is distinct from PrincipalFromContext, which on the agent plane is the
// AGENT making the call: an agent has no mailbox. The run principal is resolved
// by the kernel from facts on the presented lease (the ADR-0139 D14 binding:
// conversation owner → task beneficiary → configured default) or, on the
// operator plane, is the authenticated operator. It is never read from a
// request payload, model output or a header a caller sets (INV-5): it selects
// whose credential is used, so a forgeable value would be a credential-theft
// primitive.
type runPrincipalCtxKey struct{}

// WithRunPrincipal returns a child context carrying the run principal id.
func WithRunPrincipal(ctx context.Context, principalID string) context.Context {
	return context.WithValue(ctx, runPrincipalCtxKey{}, principalID)
}

// RunPrincipalFromContext returns the run principal id, "" when none was
// resolved. Consumers that select a per-user credential treat "" fail-closed.
func RunPrincipalFromContext(ctx context.Context) string {
	v, _ := ctx.Value(runPrincipalCtxKey{}).(string)
	return v
}

// actForActorCtxKey carries the authenticated human who is acting ON BEHALF OF
// the run principal (operator act-for, OpenConversation/SendTurn on_behalf_of),
// so work done for the person can be audited as "for <person>, by <operator>"
// (T-20261107-a-person-s-assistant-cannot-save-that-person-s). It is an AUDIT
// fact only: it never selects a scope, a credential or whose material is read or
// written — the run principal does that. Set by the operator plane after the
// act-for checks, never from a payload (INV-5).
type actForActorCtxKey struct{}

// WithActForActor returns a child context naming the acting operator. "" leaves
// ctx unchanged.
func WithActForActor(ctx context.Context, actor string) context.Context {
	if actor == "" {
		return ctx
	}
	return context.WithValue(ctx, actForActorCtxKey{}, actor)
}

// ActForActorFromContext returns the operator acting on behalf of the run
// principal, "" when the principal acts as themself.
func ActForActorFromContext(ctx context.Context) string {
	v, _ := ctx.Value(actForActorCtxKey{}).(string)
	return v
}
