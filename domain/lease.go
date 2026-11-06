package domain

// LeaseHeader is the gRPC metadata key that carries a BudgetLease, in both
// directions: an agent presents its lease to the kernel under it, and the kernel
// hands a lease to an agent under it on RPCs whose request message has no lease
// field (VerifyOutput). One name, so the two directions cannot drift apart.
const LeaseHeader = "x-lease-id"

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
// A lease with no AgentID is usable by NON-agent principals only: an agent-plane caller
// may present only a lease bound to itself (ADR-0139 D8, fail closed;
// T-20261104-leases-minted-with-no-agentid-are-usable-by-any). Every site that hands a
// lease to an agent therefore binds its AgentID at mint or dispatch.
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
	// ParentLeaseID is set when this lease belongs to an ADR-0139 D12 subagent: the
	// lease of the parent that spawned it. Non-empty marks the holder as a CHILD.
	// D12: children get no memory write (and no clarify or messaging) unless the
	// spawn granted it, so a child lease is refused memory-note writes
	// (MEMORY_NOTE_REJECTION_SCOPE_DENIED) unless MemoryWriteGranted is set.
	// Written by the kernel at spawn, never by the agent.
	ParentLeaseID LeaseID
	// MemoryWriteGranted is the D12 explicit grant of memory write to a child
	// (the spawn's toolset named the memory-write tool). Meaningless on a root lease.
	MemoryWriteGranted bool
}

// IsChild reports whether the lease belongs to a D12 subagent.
func (b LeaseBinding) IsChild() bool { return b.ParentLeaseID != "" }

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

// LeaseAgentBinder records WHICH agent a lease has been handed to, keeping the rest of the
// binding (session, run, step, conversation) untouched. Implemented by the substrate LLM
// gateway; optional, like LeaseResolver, so a gateway or fake may omit it — but an
// agent-unbound lease is refused to every agent (ADR-0139 D8), so a dispatch path whose
// gateway cannot bind hands its agent a lease that agent cannot use.
//
// Why a separate method rather than a second BindLease (ADR-0139 D8,
// T-20261104-plan-step-leases-are-bound-with-no-agentid-so): a plan-step lease is minted
// before selection picks the winner, so the executing agent is only known at the moment
// the dispatcher calls it. BindLease replaces the binding whole; the dispatcher does not
// hold the session/run/step it would have to restate, and restating it would be one more
// copy to drift.
//
// The AgentID it writes MOVES with the dispatch: the kernel hands one step lease serially
// from agent to agent (a heal retry that explores a different winner, a runner-up
// fallback, a yield delegate and then the resumed yielder), and the lease must name the
// agent executing NOW. The displaced agent's call has already returned, so it losing the
// lease is the intended effect. Called only by kernel dispatch code, never from the wire.
type LeaseAgentBinder interface {
	// BindLeaseAgent binds leaseID to agentID and reports whether the lease is known.
	// A no-op returning false for an unknown, completed or empty lease ID, or an empty
	// agentID.
	BindLeaseAgent(leaseID LeaseID, agentID string) bool
}
