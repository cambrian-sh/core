package network

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// fixedConvSurfaces records the surface a conversation arrived on, as
// ingress.ConversationSurfaces does from the conversation row.
type fixedConvSurfaces map[string]domain.SurfaceRef

func (f fixedConvSurfaces) ConversationSurface(_ context.Context, id string) (domain.SurfaceRef, bool) {
	s, ok := f[id]
	return s, ok
}

var telegram = domain.SurfaceRef{Kind: domain.SurfaceChat, ID: "chat:telegram"}

// T-20261113-surface-clamp-is-applied-only-on-querymemory: the conversation's
// recorded surface (ADR-0085 D7) is re-stamped for EVERY lane of a turn that goes
// through the shared identity hop — ExecuteTool, ListTools, the context-node RPCs —
// not only QueryMemory. Before, a chat turn's tool call was authorised as the
// transport surface agent/grpc and a surface-linked policy never saw it.
func TestWithCallerSession_RestampsTheConversationSurface(t *testing.T) {
	t.Parallel()
	s, gw := newLeaseServer(t)
	s.ConvSurfaces = fixedConvSurfaces{"conv-tg": telegram}

	// A chat turn: a conversation on the lease, no task session.
	lease := bindNotesLease(t, gw, domain.LeaseBinding{AgentID: "chat_agent", ConversationID: "conv-tg"})
	ctx := s.withCallerSession(agentPlaneCtx("chat_agent", leaseHeader, string(lease)))
	if got := domain.SurfaceFromContext(ctx); got != telegram {
		t.Fatalf("tool-call surface = %+v, want the conversation's %+v", got, telegram)
	}
	if _, ok := domain.SessionIDFromContext(ctx); ok {
		t.Fatalf("the surface clamp seeded a session on a session-less turn")
	}

	// A turn with neither session nor conversation keeps the transport surface.
	plain := s.withCallerSession(agentPlaneCtx("chat_agent"))
	if got := domain.SurfaceFromContext(plain); got != (domain.SurfaceRef{Kind: domain.SurfaceAgent, ID: "grpc"}) {
		t.Fatalf("plain call surface = %+v, want the transport's", got)
	}
}

// The clamp only narrows to a RECORDED surface: an unknown conversation keeps the
// transport surface, and a nil session manager is not dereferenced.
func TestWithTurnSurface_UnrecordedConversationKeepsTheTransport(t *testing.T) {
	t.Parallel()
	s, gw := newLeaseServer(t)
	s.ConvSurfaces = fixedConvSurfaces{}
	lease := bindNotesLease(t, gw, domain.LeaseBinding{AgentID: "chat_agent", ConversationID: "conv-unknown", SessionID: "ses-1"})
	ctx := s.withCallerSession(agentPlaneCtx("chat_agent", leaseHeader, string(lease)))
	if got := domain.SurfaceFromContext(ctx); got != (domain.SurfaceRef{Kind: domain.SurfaceAgent, ID: "grpc"}) {
		t.Fatalf("surface = %+v, want the transport's", got)
	}
}
