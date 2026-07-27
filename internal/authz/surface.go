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
//   - the operator console → SurfaceOperator (a human at the console)
//   - the agent plane      → SurfaceAgent    (an agent under its own identity)
//   - a premium plane      → SurfaceOperator (mounted behind the same operator
//     interceptors, so it is the operator plane extended, not a new privilege)
//   - anything else        → SurfaceInternal (an in-process kernel path)
func SurfaceForMethod(fullMethod string) domain.SurfaceRef {
	switch {
	case strings.HasPrefix(fullMethod, operatorServicePrefix):
		return domain.SurfaceRef{Kind: domain.SurfaceOperator, ID: "console"}
	case strings.HasPrefix(fullMethod, agentServicePrefix):
		return domain.SurfaceRef{Kind: domain.SurfaceAgent, ID: "grpc"}
	case strings.HasPrefix(fullMethod, premiumServicePrefix):
		return domain.SurfaceRef{Kind: domain.SurfaceOperator, ID: "premium-plane"}
	default:
		return domain.SurfaceRef{Kind: domain.SurfaceInternal}
	}
}

// UnarySurfaceInterceptor stamps the transport-derived surface onto every unary
// request context. It runs unconditionally — the kernel always establishes WHERE
// a request came from, and whether that constrains anything is the decision
// point's business.
func UnarySurfaceInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		return handler(domain.WithSurface(ctx, SurfaceForMethod(info.FullMethod)), req)
	}
}

// StreamSurfaceInterceptor is the streaming counterpart.
func StreamSurfaceInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		return handler(srv, &surfaceStream{ServerStream: ss, ctx: domain.WithSurface(ss.Context(), SurfaceForMethod(info.FullMethod))})
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
