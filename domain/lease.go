package domain

// LeaseBinding is the execution identity the kernel attaches to a BudgetLease at step
// dispatch. It is the server-side half of the Phase-0 identity fix: an agent presents only
// the opaque lease it was handed, and the kernel resolves the lease to WHO and WHAT it
// belongs to — rather than trusting the agent to name its own session.
//
// Why this exists: the agent SDK historically sent the per-step lease ID in the
// `x-session-id` gRPC header, and the kernel read that header as the durable task
// SessionID. Two identifiers with different lifetimes (one step vs. many plans) shared one
// wire slot, so every consumer keyed on it — caller-scope re-derivation (ADR-0034 D13),
// content-node ownership (ADR-0048 D4), and the same-session step-record filter
// (ADR-0048 D1) — was keyed on the wrong thing and silently inert.
//
// The binding is written by the kernel at Acquire time and is never accepted from the wire.
// A lease with no binding resolves to the zero value, which callers treat as "unknown" and
// fall back to legacy behaviour — so an unbound lease degrades rather than misattributes.
type LeaseBinding struct {
	// SessionID is the durable task session this lease was issued under. Empty when the
	// lease was minted outside a session (scout/retrieval/chat dispatch today).
	SessionID SessionID
	// RunID is the plan execution the lease belongs to. Carried now so the Run promotion
	// does not have to re-thread every dispatch site later.
	RunID RunID
	// StepIndex is the step within the run; -1 when the lease is not step-scoped.
	StepIndex int
	// AgentID is the agent the lease was dispatched to, when known at Acquire time.
	AgentID string
	// ConversationID and OriginMessageID are set when the lease was issued for a chat turn.
	// They travel on the LEASE rather than in client-supplied metadata for the same reason
	// the session does: the kernel must be able to attribute work to a conversation without
	// trusting the agent to name one it was not dispatched under.
	ConversationID  string
	OriginMessageID string
}

// IsZero reports whether the binding carries no identity at all. A zero binding must never
// be used to grant ownership or narrow a scope — it means "this lease was never bound".
func (b LeaseBinding) IsZero() bool {
	return b.SessionID == "" && b.RunID == "" && b.AgentID == ""
}

// LeaseResolver resolves an opaque BudgetLease ID to the identity the kernel bound to it.
// Implemented by the substrate LLM gateway, which already owns the lease registry.
//
// It is deliberately a narrow, optional port: transports type-assert for it and fall back
// to legacy header handling when the wired gateway does not implement it, so adding lease
// resolution never breaks a build that has its own gateway or a test fake.
type LeaseResolver interface {
	// BindLease attaches execution identity to an already-issued lease. Called by the
	// kernel at dispatch, never from the wire. A no-op for an unknown lease ID.
	BindLease(leaseID LeaseID, b LeaseBinding)
	// ResolveLease returns the identity bound to leaseID. The bool reports whether the
	// lease is known at all — an expired or never-issued lease returns false, which is
	// what lets a transport distinguish "stale SDK sent a lease" from "operator sent a
	// real session ID".
	ResolveLease(leaseID LeaseID) (LeaseBinding, bool)
}
