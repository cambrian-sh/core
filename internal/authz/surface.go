package authz

import (
	"context"
	"strings"

	"github.com/cambrian-sh/core/domain"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
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
// RESIDUAL RISK, stated plainly: deriving the surface from the transport is only
// as trustworthy as the transport. Until SEC-03 lands TLS + client authentication
// on the operator plane, a remote attacker who can reach the port can present
// themselves on whichever plane they like. On localhost (the shipped default) the
// transport is the process boundary and the derivation holds. Do not rely on the
// surface clamp as a security boundary on a remotely-reachable deployment until
// SEC-03 is done — the spec says so, and so does this comment.
// ─────────────────────────────────────────────────────────────────────────────

// gRPC service prefixes the kernel serves. The surface follows the SERVICE, which
// is a property of the connection's routing rather than of anything the caller
// wrote in the message body.
const (
	operatorServicePrefix = "/cambrian.OperatorConsole/"
	agentServicePrefix    = "/cambrian.Orchestrator/"
	premiumServicePrefix  = "/cambrian.premium."
)

// SurfaceForMethod derives the surface from the gRPC method being invoked.
//
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

// seedAgentPrincipal stamps the caller principal from x-agent-id metadata for
// agent-facing premium services (ADR-0118 D3) — the interceptor equivalent of
// what individual core handlers (ingest_memory) already do by hand. Kernel-side
// on purpose (INV-5): the handler never reads its own identity claims. The
// metadata itself is as trustworthy as the agent plane's transport (the SEC-03
// residual), no more and no less.
func seedAgentPrincipal(ctx context.Context, fullMethod string, agentPlane map[string]bool) context.Context {
	if len(agentPlane) == 0 || !strings.HasPrefix(fullMethod, premiumServicePrefix) ||
		!agentPlane[serviceNameOf(fullMethod)] {
		return ctx
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ctx
	}
	if vals := md.Get("x-agent-id"); len(vals) > 0 && vals[0] != "" {
		return domain.WithPrincipal(ctx, domain.AgentPrincipal(vals[0]))
	}
	return ctx
}

// UnarySurfaceInterceptor stamps the transport-derived surface onto every unary
// request context (and, for declared agent-facing premium services, the caller
// principal). It runs unconditionally — the kernel always establishes WHERE a
// request came from, and whether that constrains anything is the decision
// point's business.
func UnarySurfaceInterceptor(agentPlane map[string]bool) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx = domain.WithSurface(ctx, SurfaceForMethod(info.FullMethod, agentPlane))
		return handler(seedAgentPrincipal(ctx, info.FullMethod, agentPlane), req)
	}
}

// StreamSurfaceInterceptor is the streaming counterpart.
func StreamSurfaceInterceptor(agentPlane map[string]bool) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx := domain.WithSurface(ss.Context(), SurfaceForMethod(info.FullMethod, agentPlane))
		ctx = seedAgentPrincipal(ctx, info.FullMethod, agentPlane)
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
