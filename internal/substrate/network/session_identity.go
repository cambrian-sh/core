package network

import (
	"context"
	"log/slog"

	"google.golang.org/grpc/metadata"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
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
	leaseHeader   = domain.LeaseHeader
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

// callerAgentIdentity reports whether the inbound caller is an AGENT, and which one.
//
// The authenticated principal decides when there is one: the agent-plane interceptor
// (internal/authz/agent_auth.go) stamps domain.AgentPrincipal after checking the
// credential (or, under the ADR-0130 D2 waiver, from x-agent-id). Any other principal
// kind — the operator's UserPrincipal, a system principal, an MCP worker machine — is
// not an agent. With no principal, the surface decides: a call that arrived on the agent
// plane is an agent even when it could not say which one, and an unsurfaced in-process
// call that carries x-agent-id is treated as that agent, so the checks below still bite
// where the interceptor did not run.
func callerAgentIdentity(ctx context.Context) (agentID string, isAgent bool) {
	p := domain.PrincipalFromContext(ctx)
	if p.Kind == domain.PrincipalAgent {
		return p.ID, true
	}
	if p.ID != "" || p.Kind != "" {
		return "", false
	}
	asserted := agentIDFromMetadata(ctx)
	switch domain.SurfaceFromContext(ctx).Kind {
	case domain.SurfaceAgent:
		return asserted, true
	case "", domain.SurfaceInternal:
		return asserted, asserted != ""
	}
	return "", false
}

// resolveLeaseFor resolves a lease ON BEHALF OF the inbound caller (ADR-0139 D8: leases
// are bound to the issuing agent). It is the one place a transport turns a presented lease
// into identity, so every lane — header, legacy header, handoff payload, conversation
// lookup — applies the same ownership rule, and it FAILS CLOSED for agents:
//
//   - an AGENT caller (callerAgentIdentity isAgent) resolves a lease only when the
//     binding names that same agent. A lease bound to another agent, and a lease bound
//     to NO agent, both get (zero, false) — the same answer as for a lease that was
//     never issued, so the caller learns nothing about whether the value was live.
//     Every site that hands a lease to an agent binds it first: plan steps at dispatch
//     (dispatch.Dispatcher.bindLease → BindLeaseAgent), chat turns (TurnService.
//     SetLeaseBinder), the bypass arm, retrieval dispatch, verification and
//     the plugin lane (app.KernelServices.AcquireAgentLLMToken).
//     T-20261104-leases-minted-with-no-agentid-are-usable-by-any: the earlier rule let
//     ANY agent use an unbound lease, and an E2E run generated text with a forged agent
//     principal on a live unbound lease captured from the operator feed.
//   - a NON-agent principal (the operator's UserPrincipal, system, an MCP machine, an
//     unsurfaced in-process kernel call with no x-agent-id) resolves every live lease,
//     bound or not, exactly as before. The operator plane never presents an agent's lease
//     over the wire — it sends a real session id (app.go SendFn / planSubmitter) — and the
//     exemption keeps a lease a chat turn mints usable by the console that ordered it.
func (s *Server) resolveLeaseFor(ctx context.Context, lease domain.LeaseID) (domain.LeaseBinding, bool) {
	b, known := resolveLeaseForCaller(ctx, s.leaseResolver(), lease)
	if known {
		// ADR-0157 D5.2: the lease a verified instance works under is the run
		// provenance of what it hands back later (uploads, out/ at exit).
		s.instanceWork().note(domain.AgentInstanceFromContext(ctx), b)
	}
	return b, known
}

// resolveLeaseForCaller is resolveLeaseFor over an explicit resolver, for components that
// hold the gateway but not the Server (RetrievalDispatcher's parent-lease charging).
func resolveLeaseForCaller(ctx context.Context, resolver domain.LeaseResolver, lease domain.LeaseID) (domain.LeaseBinding, bool) {
	if resolver == nil || lease == "" {
		return domain.LeaseBinding{}, false
	}
	binding, known := resolver.ResolveLease(lease)
	if !known {
		return domain.LeaseBinding{}, false
	}
	caller, isAgent := callerAgentIdentity(ctx)
	if !isAgent {
		return binding, true
	}
	// An agent must be named by the binding. Guarding the empty AgentID explicitly
	// matters: an agent-plane caller that could not say who it is (caller == "")
	// would otherwise "match" an unbound lease.
	if binding.AgentID != "" && caller == binding.AgentID {
		return binding, true
	}
	if binding.AgentID == "" {
		slog.Warn("ADR-0139 D8: refusing an agent-unbound lease presented by an agent",
			"caller_agent", caller)
	} else {
		slog.Warn("ADR-0139 D8: refusing a lease presented by an agent it was not issued to",
			"caller_agent", caller, "lease_agent", binding.AgentID)
	}
	return domain.LeaseBinding{}, false
}

// leaseForeignToCaller reports whether lease is a LIVE lease the caller may not use: for
// an agent caller, one bound to another agent OR to no agent at all (resolveLeaseFor's
// fail-closed rule); for a non-agent principal, never. Used by the RPCs that spend or act
// under the lease directly (the model stream, GenerateWithTools, ExecuteTool, the ListTools
// overlay) to refuse rather than proceed on a stolen credential. A lease that is not live
// is not "foreign": those RPCs answer it through the gateway's own unknown-lease error.
func (s *Server) leaseForeignToCaller(ctx context.Context, lease domain.LeaseID) bool {
	resolver := s.leaseResolver()
	if resolver == nil || lease == "" {
		return false
	}
	if _, known := resolver.ResolveLease(lease); !known {
		return false
	}
	_, ok := s.resolveLeaseFor(ctx, lease)
	return !ok
}

// callerOverlayToken returns the lease the caller presented in x-lease-id, as the
// run-overlay session token (ADR-0046 D6), or "" when there is none or the caller may
// not use it (bound to another agent, or — for an agent caller — bound to none). The overlay is keyed by the lease ExecuteTool receives, so the
// describe path (ListTools) must use the same key; a stolen lease must not reveal,
// or later describe, another agent's skill-conferred tools.
func (s *Server) callerOverlayToken(ctx context.Context) string {
	lease := domain.LeaseID(mdValue(ctx, leaseHeader))
	if lease == "" || s.leaseForeignToCaller(ctx, lease) {
		return ""
	}
	return string(lease)
}

// legacySessionFallbackAllowed reports whether a raw `x-session-id` value that is not a
// lease may be taken as a session id. Only the callers that legitimately send a REAL
// session id get the permissive branch:
//
//   - the operator surface (the console; also the operator SendFn and plan submitter,
//     which since ENG-1378 run Execute on the operator's detached request ctx, surface
//     operator, with x-session-id set by app.operatorRunContext);
//   - the MCP task lane (surface mcp), which builds x-session-id itself from the session
//     it just opened — an MCP client never sets gRPC metadata; and
//   - an unsurfaced in-process kernel call that is not acting as an agent.
//
// Everything else — above all the agent plane, where the surface interceptor always
// stamps SurfaceAgent — must present a lease. An agent that could name a session by its
// raw id would inherit its surface, its content nodes and its task beneficiary.
func legacySessionFallbackAllowed(ctx context.Context) bool {
	switch domain.SurfaceFromContext(ctx).Kind {
	case domain.SurfaceOperator, domain.SurfaceMCP:
		return true
	case "", domain.SurfaceInternal:
		_, isAgent := callerAgentIdentity(ctx)
		return !isAgent
	}
	return false
}

// callerConversation returns the conversation this caller is working for, or "".
func (s *Server) callerConversation(ctx context.Context) string {
	b, ok := s.resolveCallerBinding(ctx)
	if !ok {
		return ""
	}
	return b.ConversationID
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
// until it is removed). The permissive branch is NOT open to agents
// (legacySessionFallbackAllowed, ADR-0139 D8): an agent-plane caller whose x-session-id
// is not a known lease gets "", never its raw string as a session. Only the operator
// surface, the MCP task lane and non-agent in-process calls may name a session by id.
// Every lease, on either header, resolves through resolveLeaseFor, so a lease an agent
// caller does not own (bound to another agent, or unbound) is treated as unknown.
func (s *Server) resolveCallerSession(ctx context.Context) domain.SessionID {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	resolver := s.leaseResolver()

	if lease := domain.LeaseID(firstMD(md, leaseHeader)); lease != "" {
		binding, known := s.resolveLeaseFor(ctx, lease)
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
		if _, known := resolver.ResolveLease(domain.LeaseID(legacy)); known {
			// A stale SDK put its lease here. Its bound session is the answer — and an
			// unbound lease correctly yields "" rather than the lease ID masquerading as
			// a session. A lease bound to ANOTHER agent yields "" as well, and is never
			// re-read as a raw session id below: it is a known lease either way.
			binding, ok := s.resolveLeaseFor(ctx, domain.LeaseID(legacy))
			if !ok {
				return ""
			}
			return binding.SessionID
		}
	}
	if !legacySessionFallbackAllowed(ctx) {
		return ""
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
	lease := firstMD(md, leaseHeader)
	if lease == "" {
		lease = firstMD(md, sessionHeader) // legacy slot may carry a lease
	}
	return s.resolveLeaseFor(ctx, domain.LeaseID(lease))
}

// callerLeaseID is the lease the caller presented: the gRPC header first (then the
// legacy session slot), else the handoff metadata's _session_token_id — the same
// order resolveBindingFromHandoff trusts.
func callerLeaseID(ctx context.Context, meta map[string]string) domain.LeaseID {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if l := firstMD(md, leaseHeader); l != "" {
			return domain.LeaseID(l)
		}
		if l := firstMD(md, sessionHeader); l != "" {
			return domain.LeaseID(l)
		}
	}
	if meta == nil {
		return ""
	}
	return domain.LeaseID(meta["_session_token_id"])
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
	if meta == nil {
		return domain.LeaseBinding{}, false
	}
	return s.resolveLeaseFor(ctx, domain.LeaseID(meta["_session_token_id"]))
}

// TaskBeneficiarySource resolves a task session to the beneficiary owner
// principal the task is being done FOR (ADR-0126 phase 4 / ADR-0127 D4).
// Implemented by the MCP task lane's in-memory index at the composition root;
// zero means "this task carries no beneficiary", which every consumer treats
// fail-closed (no beneficiary ⇒ no contributed fleet).
type TaskBeneficiarySource interface {
	BeneficiaryOf(task domain.SessionID) domain.PrincipalRef
}

// withCallerSession threads the resolved session into ctx for the read/write chokepoints
// that look it up (scope re-derivation, content-node ownership, the same-session step
// filter). A caller with no session leaves ctx untouched, so "no session" stays
// distinguishable from "session with an empty ID".
//
// It ALSO seeds the task's beneficiary owner principal (ADR-0126 phase 4):
// resolved lease → task session → TaskOwners index, kernel-side end to end, so
// menu building (AvailableTools → contributedTools) and the dispatch-side
// ownership check both start from a fact no caller header can assert (INV-5).
// A session with no beneficiary seeds nothing and contributed resolution stays
// fail-closed — the pre-phase-4 behaviour for every non-task session.
func (s *Server) withCallerSession(ctx context.Context) context.Context {
	ctx = s.withConversationOwner(ctx)
	sid := s.resolveCallerSession(ctx)
	if sid == "" {
		// A chat turn has a conversation and no session; its surface clamp still applies.
		return s.withTurnSurface(ctx, "")
	}
	ctx = domain.WithSessionID(ctx, sid)
	ctx = s.withTurnSurface(ctx, sid)
	if s.TaskOwners != nil {
		if owner := s.TaskOwners.BeneficiaryOf(sid); !owner.IsZero() {
			ctx = domain.WithTaskBeneficiary(ctx, owner)
		}
	}
	// ADR-0127 CL-2 / ADR-0142 D11: the ordering conversation, so an approval or
	// consent prompt raised at the tool chokepoint names (and can be delivered
	// to) the conversation that ordered the work (ADR-0098). Same trust rule as
	// everything above: only the lease binding is believed, never a header a
	// caller could set. A chat turn's lease carries the conversation itself; a
	// plan step's lease carries only its task session, whose conversation link
	// (ADR-0084 D2) is the second hop — without it every approval raised by a
	// chat-delegated plan step named no conversation
	// (T-20261107-tool-approvals-raised-by-chat-delegated-plan).
	if b, known := s.resolveCallerBinding(ctx); known {
		if conv := s.conversationOfBinding(ctx, b); conv != "" {
			ctx = domain.WithConversationID(ctx, conv)
		}
	}
	return ctx
}

// withTurnSurface is the ADR-0085 D7 surface clamp for one agent-plane turn: a
// recorded surface OVERRIDES the transport-derived one — the task session's,
// else the conversation's (authz.ResolveSurfaceForTurn), both read server-side
// from a persisted record keyed by the kernel-minted lease, never from client
// metadata (INV-5). A conversation opened on an outsider ingress stays an
// outsider conversation even when a later turn arrives over an internal path;
// widening on the way in is the escalation the clamp exists to prevent.
//
// ONE helper for every lane of the turn — QueryMemory, ExecuteTool/ListTools and
// the context-node RPCs (through withCallerSession), ListSkills — so a
// surface-linked policy binds tool calls, kernel-tool reads and skill menus
// exactly as it binds recall. It used to be stamped on QueryMemory only
// (T-20261113-surface-clamp-is-applied-only-on-querymemory). Resolved whenever
// the caller has a session OR a conversation: a chat turn has the latter only.
// Only the surface is added to the returned ctx: the session id is used for the
// lookup, never seeded here (that is withCallerSession's decision).
func (s *Server) withTurnSurface(ctx context.Context, sessionID domain.SessionID) context.Context {
	convID := s.callerConversation(ctx)
	if sessionID == "" && convID == "" {
		return ctx
	}
	var sessions authz.SessionSurfaceReader
	if s.SessionMgr != nil { // a typed-nil manager would panic inside SessionSurface
		sessions = s.SessionMgr
	}
	lookup := ctx
	if sessionID != "" {
		lookup = domain.WithSessionID(ctx, sessionID)
	}
	return domain.WithSurface(ctx, authz.ResolveSurfaceForTurn(lookup, sessions, s.ConvSurfaces, convID))
}

// withPresentedLease makes the lease an RPC carries in its REQUEST BODY the lease
// every header-based resolution in this file reads (resolveCallerSession,
// resolveCallerBinding, withConversationOwner, resolveCallerConversation).
//
// T-20261107-tool-approvals-raised-by-chat-delegated-plan: the SDK's execute_tool
// sends the per-step lease ONLY as ExecuteToolRequest.lease_id, never as the
// x-lease-id header. ExecuteTool authorised on the body lease (foreign-lease
// check, run principal, turn guard, subagent admission) but resolved the task
// session, conversation and conversation owner from the headers — so for every
// SDK tool call they were all empty, and a dangerous-tool approval raised from a
// plan step named no beneficiary and was unroutable under any self or
// relationship rule. Projecting the body lease onto the header makes the one
// lease the request presents the only identity input, resolved by the same code
// and under the same ADR-0139 D8 ownership rule as every other lane: a lease the
// caller may not use still resolves to nothing.
//
// The body lease is authoritative because it is the one the RPC already
// authorises on; a different x-lease-id header is overwritten (and logged), so a
// call can never be authorised as one lease and attributed to another.
func withPresentedLease(ctx context.Context, lease domain.LeaseID) context.Context {
	if lease == "" {
		return ctx
	}
	md, _ := metadata.FromIncomingContext(ctx)
	if prior := firstMD(md, leaseHeader); prior == string(lease) {
		return ctx
	} else if prior != "" {
		slog.Warn("x-lease-id header differs from the request's lease; the request's lease is used",
			"agent", agentIDFromMetadata(ctx))
	}
	md = md.Copy()
	md.Set(leaseHeader, string(lease))
	return metadata.NewIncomingContext(ctx, md)
}

// conversationOfBinding is the conversation a resolved lease binding works for:
// on the lease itself (a chat turn), else on its task session (a plan step a
// chat turn ordered — the ADR-0084 D2 link). "" when neither names one.
func (s *Server) conversationOfBinding(ctx context.Context, b domain.LeaseBinding) string {
	if b.ConversationID != "" {
		return b.ConversationID
	}
	if b.SessionID == "" || s.SessionMgr == nil {
		return ""
	}
	ses, err := s.SessionMgr.GetSession(ctx, b.SessionID)
	if err != nil || ses == nil {
		return ""
	}
	return ses.ConversationID
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
	lease := domain.LeaseID(firstMD(md, leaseHeader))
	if lease == "" {
		lease = domain.LeaseID(firstMD(md, sessionHeader)) // stale SDKs put the lease here
	}
	binding, known := s.resolveLeaseFor(ctx, lease)
	if !known {
		return ""
	}
	// Only the chat turn's OWN lease carries a conversation. The moment that turn delegates
	// to the planner, each step runs under a fresh step lease bound to a SessionID — so the
	// agents doing the slow work (retrieval, tools, file writes) would resolve to no
	// conversation at all, and the user watching would see one static line while the system
	// is busiest. conversationOfBinding takes the second hop through the session's ADR-0084
	// D2 link, so this attribution is possible server-side, without an agent naming anything.
	return s.conversationOfBinding(ctx, binding)
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

// liveCallerSession is resolveCallerSession narrowed to a session that exists
// and is still open — the session Execute will continue — WITHOUT creating
// one. Execute uses it to put router classification under the caller's
// session (ADR-0131, owner decision 2026-11-01) before the full resolution
// below it, which may open a new session; "" when the caller presents none, or
// one that is unknown or completed.
func (s *Server) liveCallerSession(ctx context.Context) domain.SessionID {
	if s.SessionMgr == nil {
		return ""
	}
	sid := s.resolveCallerSession(ctx)
	if sid == "" {
		return ""
	}
	ses, err := s.SessionMgr.GetSession(ctx, sid)
	if err != nil || ses == nil || ses.Status == domain.SessionCompleted {
		return ""
	}
	return ses.ID
}

// withConversationOwner stamps the owner of the conversation the caller's lease
// was issued under (domain.WithConversationOwner) — on the lease itself, or on its
// task session — so the decision point narrows the agent's reads and calls by what
// that person may see (T-20261106-conversations-cannot-be-opened-as-a-given).
// Kernel-held facts only: the lease binding and the conversation record, never a
// header the caller sets. Runs BEFORE the "no session" early return in
// withCallerSession because a chat turn's lease carries a conversation and no
// session. No lease, no conversation or no resolver ⇒ ctx unchanged (the agent's
// own scope governs, as before).
func (s *Server) withConversationOwner(ctx context.Context) context.Context {
	ctx = s.markResolvedLease(ctx, callerLeaseID(ctx, nil))
	if s.ConversationOwner == nil || domain.ConversationOwnerFromContext(ctx) != "" {
		return ctx
	}
	b, known := s.resolveCallerBinding(ctx)
	return s.stampConversationOwner(ctx, b, known)
}

// withConversationOwnerFromHandoff is withConversationOwner for Execute, whose
// caller may present its lease INSIDE the handoff (_session_token_id) rather than
// as a header — the SDK's delegate does (ENG-1029, owner decision 2026-11-14:
// Execute stamps the conversation owner from the handoff lease, like header
// leases, so the decision point narrows the agent by the owner's terms and the
// owner fence lets the person's own rows through). The lease is still resolved
// kernel-side and must be bound to the calling agent (resolveBindingFromHandoff
// → resolveLeaseFor, ADR-0139 D8); a header lease still wins.
func (s *Server) withConversationOwnerFromHandoff(ctx context.Context, meta map[string]string) context.Context {
	ctx = s.markResolvedLease(ctx, callerLeaseID(ctx, meta))
	if s.ConversationOwner == nil || domain.ConversationOwnerFromContext(ctx) != "" {
		return ctx
	}
	b, known := s.resolveBindingFromHandoff(ctx, meta)
	return s.stampConversationOwner(ctx, b, known)
}

// markResolvedLease stamps domain.WithResolvedLease when lease resolves to a
// binding for the calling agent (resolveLeaseFor applies the ADR-0139 D8
// ownership rule), so the decision point can tell "no lease presented" from
// "lease resolved" exactly (ENG-518) rather than inferring it from the session,
// owner and conversation it may or may not carry.
func (s *Server) markResolvedLease(ctx context.Context, lease domain.LeaseID) context.Context {
	if lease == "" {
		return ctx
	}
	if _, already := domain.ResolvedLeaseFromContext(ctx); already {
		return ctx
	}
	if _, known := s.resolveLeaseFor(ctx, lease); known {
		return domain.WithResolvedLease(ctx, lease)
	}
	return ctx
}

func (s *Server) stampConversationOwner(ctx context.Context, b domain.LeaseBinding, known bool) context.Context {
	if !known {
		return ctx
	}
	// ENG-614 (ADR-0084 D7): the conversation's PROFILE rides the same lease lookup,
	// stamped whether or not the conversation has an owner (a customer conversation
	// from an ingress may have none). The decision point narrows a customer
	// conversation's reads to customer-visible tags (authz.ProfileNarrowingAuthorizer).
	if s.ConversationProfile != nil {
		ctx = domain.WithConversationProfile(ctx, s.ConversationProfile(ctx, b))
	}
	if owner := s.ConversationOwner(ctx, b); owner != "" {
		ctx = domain.WithConversationOwner(ctx, owner)
		if s.PrivateByDefault {
			// ADR-0142 D12: what the agent writes for this person is theirs.
			ctx = domain.WithPrivateWrites(ctx, owner)
		}
		return ctx
	}
	return ctx
}
