package network

import (
	"context"

	"google.golang.org/grpc/metadata"

	"github.com/cambrian-sh/core/domain"
)

// Inbound identity headers.
//
// leaseHeader is what an agent presents: the opaque per-step BudgetLease the kernel handed
// it at dispatch. It is a CREDENTIAL, not a designation — the agent cannot choose its value
// and cannot name a session it was not dispatched under.
//
// sessionHeader is the deprecated legacy slot. Two different callers put two different
// things in it, which is the conflation this file exists to unwind:
//   - older agent SDKs put the LEASE there (they threaded `session_token_id` straight into
//     `x-session-id`), and
//   - the operator plane puts a REAL session ID there (app.go SendMessage).
const (
	leaseHeader   = "x-lease-id"
	sessionHeader = "x-session-id"
)

// leaseResolver returns the wired gateway's lease registry, or nil when the gateway does
// not implement resolution. Typed-nil-safe: a nil gateway yields nil rather than a
// resolver whose methods would panic.
func (s *Server) leaseResolver() domain.LeaseResolver {
	if s == nil || s.LLMGateway == nil {
		return nil
	}
	r, ok := s.LLMGateway.(domain.LeaseResolver)
	if !ok {
		return nil
	}
	return r
}

// resolveCallerSession returns the durable TASK SESSION of the inbound caller, or "" when
// the caller has none.
//
// The resolution order is the whole point of Phase 0:
//
//  1. `x-lease-id` is always a lease. Resolve it through the kernel's registry and return
//     the session the kernel bound at dispatch. An unresolvable lease returns "" — it is
//     never reinterpreted as a session ID, because an agent must not be able to name a
//     session by putting a string in a header.
//
//  2. `x-session-id` is ambiguous, so try lease resolution FIRST. If the value is a known
//     lease, the caller is a stale SDK and we return its bound session — which fixes the
//     conflation for un-upgraded agents with no coordinated deploy. Only if the value is
//     not a known lease do we treat it as a session ID, which preserves the operator plane.
//
// The asymmetry between the two branches is deliberate: the new header is strict (a lease
// is only ever a lease), the legacy header is permissive (it has to serve both callers
// until it is removed).
// callerConversation returns the conversation this caller is working for, or "".
func (s *Server) callerConversation(ctx context.Context) string {
	b, ok := s.resolveCallerBinding(ctx)
	if !ok {
		return ""
	}
	return b.ConversationID
}

func (s *Server) resolveCallerSession(ctx context.Context) domain.SessionID {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	resolver := s.leaseResolver()

	if lease := domain.LeaseID(firstMD(md, leaseHeader)); lease != "" {
		if resolver == nil {
			return ""
		}
		binding, known := resolver.ResolveLease(lease)
		if !known {
			return ""
		}
		return binding.SessionID
	}

	legacy := firstMD(md, sessionHeader)
	if legacy == "" {
		return ""
	}
	if resolver != nil {
		if binding, known := resolver.ResolveLease(domain.LeaseID(legacy)); known {
			// A stale SDK put its lease here. Its bound session is the answer — and an
			// unbound lease correctly yields "" rather than the lease ID masquerading as
			// a session.
			return binding.SessionID
		}
	}
	return domain.SessionID(legacy)
}

// resolveCallerBinding returns the FULL identity bound to the caller's lease — session, run,
// step, and the conversation that ordered the work when the caller is a chat turn.
//
// Same discipline as resolveCallerSession: only a lease is trusted, because only a lease is
// something the kernel issued. A caller cannot claim a conversation it was not dispatched
// under by putting an id in a header or a payload field.
func (s *Server) resolveCallerBinding(ctx context.Context) (domain.LeaseBinding, bool) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return domain.LeaseBinding{}, false
	}
	resolver := s.leaseResolver()
	if resolver == nil {
		return domain.LeaseBinding{}, false
	}
	lease := firstMD(md, leaseHeader)
	if lease == "" {
		lease = firstMD(md, sessionHeader) // legacy slot may carry a lease
	}
	if lease == "" {
		return domain.LeaseBinding{}, false
	}
	return resolver.ResolveLease(domain.LeaseID(lease))
}

// resolveBindingFromHandoff resolves the caller's identity when the lease arrived INSIDE
// the request rather than as a gRPC header.
//
// The SDK's delegation call (`delegate`) sends its lease as the handoff metadata field
// `_session_token_id` and sets no gRPC metadata at all. So a chat turn that delegates to
// the planner looked, to header-based resolution, like a caller with no lease — and the
// session it opened was never linked to the conversation that ordered it. Every session in
// the store carried an empty conversation_id as a result, and anything that needed to
// attribute delegated work back to a conversation silently found nothing.
//
// Header first, payload second: the header is set by the transport and the payload by the
// caller, so preferring the header keeps the more trustworthy source authoritative.
func (s *Server) resolveBindingFromHandoff(ctx context.Context, meta map[string]string) (domain.LeaseBinding, bool) {
	if b, known := s.resolveCallerBinding(ctx); known {
		return b, true
	}
	resolver := s.leaseResolver()
	if resolver == nil || meta == nil {
		return domain.LeaseBinding{}, false
	}
	lease := meta["_session_token_id"]
	if lease == "" {
		return domain.LeaseBinding{}, false
	}
	return resolver.ResolveLease(domain.LeaseID(lease))
}

// withCallerSession threads the resolved session into ctx for the read/write chokepoints
// that look it up (scope re-derivation, content-node ownership, the same-session step
// filter). A caller with no session leaves ctx untouched, so "no session" stays
// distinguishable from "session with an empty ID".
func (s *Server) withCallerSession(ctx context.Context) context.Context {
	if sid := s.resolveCallerSession(ctx); sid != "" {
		return domain.WithSessionID(ctx, sid)
	}
	return ctx
}

// firstMD returns the first value for key, or "".
func firstMD(md metadata.MD, key string) string {
	vals := md.Get(key)
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}

// leaseIDOf picks the lease from the preferred field, falling back to the deprecated one.
//
// The two proto fields carry the SAME value — a per-step BudgetLease. `lease_id` is the
// honest name; `session_token_id` is the historical one whose "session" wording is what
// let a per-step credential be read as a durable task session downstream. Both are
// accepted until the floor SDK emits `lease_id`.
func leaseIDOf(preferred, deprecated string) domain.LeaseID {
	if preferred != "" {
		return domain.LeaseID(preferred)
	}
	return domain.LeaseID(deprecated)
}

// resolveCallerConversation returns the conversation a caller's lease was issued under, or
// "" when the work is not part of a chat turn (ADR-0098).
//
// It reads the SAME binding as resolveCallerSession, for the same reason: the conversation
// travels on the lease rather than in client metadata, so the kernel can attribute work to
// a chat turn without trusting an agent to name a conversation it was not dispatched under.
//
// Used only to decide who should be told what is happening. A miss means no progress is
// reported, never that work is blocked.
func (s *Server) resolveCallerConversation(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	resolver := s.leaseResolver()
	if resolver == nil {
		return ""
	}
	lease := domain.LeaseID(firstMD(md, leaseHeader))
	if lease == "" {
		lease = domain.LeaseID(firstMD(md, sessionHeader)) // stale SDKs put the lease here
	}
	if lease == "" {
		return ""
	}
	binding, known := resolver.ResolveLease(lease)
	if !known {
		return ""
	}
	if binding.ConversationID != "" {
		return binding.ConversationID
	}

	// Second hop, and the one that actually matters in practice.
	//
	// Only the chat turn's OWN lease carries a conversation. The moment that turn delegates
	// to the planner, each step runs under a fresh step lease bound to a SessionID — so the
	// agents doing the slow work (retrieval, tools, file writes) resolve to no conversation
	// at all, and the user watching sees one static line while the system is busiest.
	//
	// ADR-0084 D2 linked the session back to the conversation that ordered it precisely so
	// this attribution is possible server-side, without an agent naming anything.
	if binding.SessionID == "" || s.SessionMgr == nil {
		return ""
	}
	ses, err := s.SessionMgr.GetSession(ctx, binding.SessionID)
	if err != nil || ses == nil {
		return ""
	}
	return ses.ConversationID
}

// reportProgress tells whoever is waiting on this caller's conversation what the kernel is
// doing on their behalf. Best-effort and silent when nothing is listening (ADR-0098 D5).
func (s *Server) reportProgress(ctx context.Context, phase domain.ProgressPhase) {
	if s.Progress == nil {
		return
	}
	conversationID := s.resolveCallerConversation(ctx)
	if conversationID == "" {
		return
	}
	domain.EmitProgress(ctx, s.Progress, domain.ProgressUpdate{
		ConversationID: conversationID,
		Phase:          phase,
	})
}
