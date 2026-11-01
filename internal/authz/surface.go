package authz

import (
	"context"
	"strings"

	"github.com/cambrian-sh/core/domain"

	"google.golang.org/grpc"
)

// ─────────────────────────────────────────────────────────────────────────────
// The surface clamp (ADR-0085 D7) — the Windows loopback analogue.
//
// In Windows: the MACHINE you sit at can override your normal permissions. Here:
// the SURFACE you arrive through can clamp what you may do, no matter who you
// are. An outsider-facing chat ingress carries a surface term forbidding
// internal_only / secrets / PII, and then — even if identity resolution is wrong,
// even if someone's group membership is misconfigured — the outsider path cannot
// reach internal knowledge. It is a structural backstop that does not depend on
// identity being correct, which is what makes a customer-facing surface shippable
// before multi-tenancy is solved.
//
// THE HARD REQUIREMENT (INV-5): the surface is established by the KERNEL, from
// the connection the request arrived on. It is never read from a request payload
// and never taken from a daemon's claim about itself. A daemon is a black box; a
// black box asserting its own privilege level is not a security boundary.
//
// The surface is a fact about the TRANSPORT and nothing more: this interceptor
// stamps where a request arrived, and it establishes no principal. WHO is
// asking is decided by the credential interceptors that run after it — the
// operator bearer (operator.UnaryAuthInterceptor) for the console and premium
// operator planes, the per-agent named secret (UnaryAgentAuthInterceptor,
// ADR-0130 D1) for the agent plane. Until ADR-0130 the agent principal was
// seeded here from the self-asserted x-agent-id header, which is the residual
// risk this comment used to describe; with server.insecure_localhost=true that
// pre-0130 behaviour is restored deliberately (D2), and the boot log says so.
// ─────────────────────────────────────────────────────────────────────────────

// gRPC service prefixes the kernel serves. The surface follows the SERVICE, which
// is a property of the connection's routing rather than of anything the caller
// wrote in the message body.
const (
	operatorServicePrefix = "/cambrian.OperatorConsole/"
	agentServicePrefix    = "/cambrian.Orchestrator/"
	premiumServicePrefix  = "/cambrian.premium."
)

// operatorGatedAgentServiceMethods are methods DECLARED on the agent-facing
// Orchestrator service that are nevertheless operator-plane: the dangerous-tool
// approval surface, which ADR-0039 D10 requires to be "operator-authenticated,
// never agent-reachable". They stay on Orchestrator to avoid a proto break, but
// they require the operator bearer (operator.authorize) and are stamped
// SurfaceOperator here. Before 2026-10-31 their only guard was an ABSENT
// self-asserted x-agent-id header, so any client reaching the port could stream
// pending approvals and approve its own dangerous tool calls.
//
// This is the ONE list; the operator auth interceptor reads it through
// IsOperatorGatedAgentServiceMethod rather than keeping its own copy.
//
// The four watch-CRUD methods are here for the same reason (2026-10-31,
// T-20261031-orchestrator-watch-crud-registerwatch): they had no guard at all.
// They are also RETIRED in place — the handlers refuse every call and name the
// audited OperatorConsole equivalent — so the gate only decides who is told so.
var operatorGatedAgentServiceMethods = map[string]bool{
	agentServicePrefix + "WatchApprovals":         true,
	agentServicePrefix + "SubmitApprovalDecision": true,
	agentServicePrefix + "RegisterWatch":          true,
	agentServicePrefix + "ListWatches":            true,
	agentServicePrefix + "DeleteWatch":            true,
	agentServicePrefix + "SetWatchActive":         true,
}

// IsOperatorGatedAgentServiceMethod reports whether fullMethod is an
// Orchestrator method that belongs to the operator plane (ADR-0039 D10).
func IsOperatorGatedAgentServiceMethod(fullMethod string) bool {
	return operatorGatedAgentServiceMethods[fullMethod]
}

// SurfaceForMethod derives the surface from the gRPC method being invoked.
//
//   - an operator-gated Orchestrator method (the approval surface) → SurfaceOperator
//   - the operator console  → SurfaceOperator (a human at the console)
//   - the agent plane       → SurfaceAgent    (an agent under its own identity)
//   - a premium plane       → SurfaceOperator (mounted behind the same operator
//     interceptors, so it is the operator plane extended, not a new privilege)
//     — UNLESS the service was declared agent-facing at registration
//     (ADR-0118 D3), in which case it IS the agent plane extended and gets
//     SurfaceAgent.
//   - anything else         → SurfaceInternal (an in-process kernel path)
func SurfaceForMethod(fullMethod string, agentPlane map[string]bool) domain.SurfaceRef {
	switch {
	case operatorGatedAgentServiceMethods[fullMethod]:
		return domain.SurfaceRef{Kind: domain.SurfaceOperator, ID: "console"}
	case strings.HasPrefix(fullMethod, operatorServicePrefix):
		return domain.SurfaceRef{Kind: domain.SurfaceOperator, ID: "console"}
	case strings.HasPrefix(fullMethod, agentServicePrefix):
		return domain.SurfaceRef{Kind: domain.SurfaceAgent, ID: "grpc"}
	case strings.HasPrefix(fullMethod, premiumServicePrefix):
		if agentPlane[serviceNameOf(fullMethod)] {
			return domain.SurfaceRef{Kind: domain.SurfaceAgent, ID: "premium-agent-plane"}
		}
		return domain.SurfaceRef{Kind: domain.SurfaceOperator, ID: "premium-plane"}
	default:
		return domain.SurfaceRef{Kind: domain.SurfaceInternal}
	}
}

// serviceNameOf extracts "pkg.Service" from "/pkg.Service/Method".
func serviceNameOf(fullMethod string) string {
	rest := strings.TrimPrefix(fullMethod, "/")
	if i := strings.Index(rest, "/"); i >= 0 {
		return rest[:i]
	}
	return rest
}

// UnarySurfaceInterceptor stamps the transport-derived surface onto every unary
// request context. It runs unconditionally — the kernel always establishes
// WHERE a request came from, and whether that constrains anything is the
// decision point's business.
//
// It no longer seeds the agent principal. Until ADR-0130 it stamped
// domain.AgentPrincipal(x-agent-id) for declared agent-facing premium
// services, which made an unverified header the identity; the principal is
// now established by UnaryAgentAuthInterceptor (agent_auth.go) for the whole
// agent plane, after the credential is checked.
func UnarySurfaceInterceptor(agentPlane map[string]bool) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx = domain.WithSurface(ctx, SurfaceForMethod(info.FullMethod, agentPlane))
		return handler(ctx, req)
	}
}

// StreamSurfaceInterceptor is the streaming counterpart.
func StreamSurfaceInterceptor(agentPlane map[string]bool) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx := domain.WithSurface(ss.Context(), SurfaceForMethod(info.FullMethod, agentPlane))
		return handler(srv, &surfaceStream{ServerStream: ss, ctx: ctx})
	}
}

type surfaceStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *surfaceStream) Context() context.Context { return s.ctx }

// SessionSurfaceReader resolves the surface a session was OPENED on, from the
// persisted session record. This is how a conversation keeps its clamp across
// turns: the surface is decided once, server-side, when the session is created,
// and every later turn inherits it — the daemon delivering the turn cannot
// restate it.
type SessionSurfaceReader interface {
	SessionSurface(ctx context.Context, sessionID domain.SessionID) (domain.SurfaceRef, bool)
}

// ConversationSurfaceReader resolves the surface a CONVERSATION arrived on, from
// its bound delivery address.
//
// It exists because a chat turn has no task session. A turn's lease carries a
// ConversationID and an empty SessionID (leases minted outside a session — chat
// dispatch, scout, retrieval), so session-based resolution finds nothing and the
// request keeps the transport-derived surface: `agent`.
//
// The consequence was not cosmetic. Every turn from a Telegram user was
// authorised as an ordinary agent call, so the policy linked to
// `surface:chat:telegram` — the lock on the door those messages came through —
// was never consulted, and no decision was ever attributed to the entry point.
type ConversationSurfaceReader interface {
	ConversationSurface(ctx context.Context, conversationID string) (domain.SurfaceRef, bool)
}

// ResolveSurface returns the surface for a request: the session's recorded
// surface when there is one, otherwise the transport-derived surface already on
// the context.
//
// The session wins because it is the NARROWER and more specific fact: a chat
// conversation opened on an outsider ingress stays an outsider conversation even
// when a later turn arrives over an internal path. Widening on the way in is
// exactly the escalation the clamp exists to prevent.
func ResolveSurface(ctx context.Context, sessions SessionSurfaceReader) domain.SurfaceRef {
	if sessions != nil {
		if sid, ok := domain.SessionIDFromContext(ctx); ok {
			if s, found := sessions.SessionSurface(ctx, sid); found && (s.Kind != "" || s.ID != "") {
				return s
			}
		}
	}
	return domain.SurfaceFromContext(ctx)
}

// ResolveSurfaceForTurn is ResolveSurface with the conversation fallback.
//
// Order is narrowest-first and deliberate: a task session's recorded surface,
// then the conversation the turn belongs to, then the transport. Both of the
// first two are read SERVER-SIDE from a persisted record — the daemon delivering
// a turn cannot restate which surface it is on, which is the whole point of the
// clamp (INV-5). The conversation id comes off the LEASE the kernel minted, not
// from client metadata, for the same reason.
//
// Widening on the way in is exactly the escalation this exists to prevent, so a
// conversation that arrived through an ingress keeps that ingress's surface even
// though the turn reaches the kernel over an ordinary agent connection.
func ResolveSurfaceForTurn(
	ctx context.Context,
	sessions SessionSurfaceReader,
	conversations ConversationSurfaceReader,
	conversationID string,
) domain.SurfaceRef {
	if sessions != nil {
		if sid, ok := domain.SessionIDFromContext(ctx); ok {
			if s, found := sessions.SessionSurface(ctx, sid); found && (s.Kind != "" || s.ID != "") {
				return s
			}
		}
	}
	if conversations != nil && conversationID != "" {
		if s, found := conversations.ConversationSurface(ctx, conversationID); found && (s.Kind != "" || s.ID != "") {
			return s
		}
	}
	return domain.SurfaceFromContext(ctx)
}
