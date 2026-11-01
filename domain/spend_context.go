package domain

import "context"

// Spend attribution carried in ctx (ADR-0131; owner decision 2026-11-01).
//
// The kernel's one LLM spend ledger (llm.SpendMeter) books every call to the
// session in ctx (WithSessionID) and refuses request-path calls once that
// session is over execution.session.max_session_tokens. Background organs —
// verification, tier-2 scoring — run after the request on their own goroutine;
// their spend still belongs to the session that caused it, but refusing them
// would silently drop the organ's job. They carry the originating session AND
// this marker: booked to the session, never refused.
//
// The marker lives in domain, beside WithSessionID, so the organs that set it
// (memory, supervision) need no import of the LLM adapter package.

type backgroundSpendKey struct{}

// WithBackgroundSpend marks LLM calls made under ctx as background-organ work:
// metered against the session in ctx, never refused at its ceiling. Combine
// with context.WithoutCancel for work that outlives the request (AGENTS.md
// §2.7) — never context.Background(), which would also drop the session.
func WithBackgroundSpend(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundSpendKey{}, true)
}

// IsBackgroundSpend reports whether ctx carries the WithBackgroundSpend marker.
func IsBackgroundSpend(ctx context.Context) bool {
	v, _ := ctx.Value(backgroundSpendKey{}).(bool)
	return v
}
