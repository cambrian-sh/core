package domain

import "context"

// scopeCtxKey is the private context key under which an EffectiveScope is carried
// through intermediate OSS helpers whose signatures must not be churned
// (memory.Manager.Query, WorkspaceStage.PrimeForStep, Agent.FetchContext). The
// boundary injects once via WithScope; the Search chokepoint reads it back.
// ADR-0034 (D5).
type scopeCtxKey struct{}

// WithScope returns a child context carrying the effective access scope.
func WithScope(ctx context.Context, scope *EffectiveScope) context.Context {
	return context.WithValue(ctx, scopeCtxKey{}, scope)
}

// ScopeFromContext returns the effective scope carried by ctx, if any. The
// boolean reports presence — a present-but-nil scope is distinct from absence,
// and both are treated fail-closed at the chokepoint.
func ScopeFromContext(ctx context.Context) (*EffectiveScope, bool) {
	v := ctx.Value(scopeCtxKey{})
	if v == nil {
		return nil, false
	}
	scope, ok := v.(*EffectiveScope)
	return scope, ok
}

// sessionIDCtxKey carries the conversation/session ID through to the read
// chokepoint so the Phase-2 caller_scope can be looked up server-side from the
// session record (never from the forgeable Handoff.Context). ADR-0034 (D13).
type sessionIDCtxKey struct{}

// WithSessionID returns a child context carrying the session ID.
func WithSessionID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, sessionIDCtxKey{}, sessionID)
}

// SessionIDFromContext returns the session ID carried by ctx, if any.
func SessionIDFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(sessionIDCtxKey{}).(string)
	return v, ok && v != ""
}
