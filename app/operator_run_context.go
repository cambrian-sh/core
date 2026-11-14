package app

import (
	"context"

	"google.golang.org/grpc/metadata"
)

// operatorRunContext is the context an operator-started run (SendMessage,
// SubmitPlan) executes on. Owner decision 2026-11-14 (ENG-1378, the same rule as
// the MCP task lane, ENG-1467): Execute carries the operator's request values —
// principal, surface, scope, trace — detached with context.WithoutCancel, because
// both RPCs return at once and the run outlives the request. The session rides
// x-session-id exactly as before (legacySessionFallbackAllowed admits it for the
// operator surface), and the incoming metadata is REPLACED, so the operator's own
// headers (its token) never reach the agent-facing Execute path. Before this the
// run started from context.Background() with only the session header.
func operatorRunContext(reqCtx context.Context, sessionID string) context.Context {
	return metadata.NewIncomingContext(context.WithoutCancel(reqCtx),
		metadata.Pairs("x-session-id", sessionID))
}
