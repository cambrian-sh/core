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
