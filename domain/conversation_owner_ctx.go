package domain

import "context"

// The conversation owner on a request context
// (T-20261106-conversations-cannot-be-opened-as-a-given).
//
// An agent working a chat turn reads and acts with its OWN principal — the
// agent's scope is the floor. But the turn is FOR the conversation's owner, and
// what the owner may see must bound what the agent shows them: a sales rep's
// assistant must not surface finance-only rows to the sales rep just because the
// assistant agent itself may read them. This key carries that owner, resolved by
// the kernel from the presented lease (conversation on the lease, or on its task
// session) — never from a header or the request — so the decision point can fold
// the owner's read predicate in as a NARROWING term, the same way a session's
// caller_scope (BRAIN-01) narrows an operator-opened session.
//
// Deliberately separate from the run principal (WithRunPrincipal): that one
// also falls back to an MCP task beneficiary or a configured default principal,
// which are not people whose entitlements were ever authored. Only a
// conversation owner is.

type conversationOwnerCtxKey struct{}

// WithConversationOwner returns a child context carrying the principal id of the
// conversation the current work is for.
func WithConversationOwner(ctx context.Context, principalID string) context.Context {
	if principalID == "" {
		return ctx
	}
	return context.WithValue(ctx, conversationOwnerCtxKey{}, principalID)
}

// ConversationOwnerFromContext returns the conversation owner, "" when the work
// is not for a conversation (or none was resolved).
func ConversationOwnerFromContext(ctx context.Context) string {
	v, _ := ctx.Value(conversationOwnerCtxKey{}).(string)
	return v
}
