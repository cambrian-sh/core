package domain

import (
	"context"
	"testing"
)

// surfaceRecordingAuthz records the surface each ReadFilter was asked on.
type surfaceRecordingAuthz struct {
	AllowAllAuthorizer
	got *SurfaceRef
}

func (a surfaceRecordingAuthz) ReadFilter(ctx context.Context, p PrincipalRef, s SurfaceRef) (*TagPredicate, AccessDecision) {
	*a.got = s
	return a.AllowAllAuthorizer.ReadFilter(ctx, p, s)
}

// T-20261113-surface-clamp-is-applied-only-on-querymemory: kernel-tool reads and
// staged inputs (callerReadScope) ask the decision point on the TURN's surface —
// the one the effect decision sees — not a hard-coded agent surface, so a policy
// linked to a chat ingress binds them too. With no surface stamped (an
// in-process path) the old agent surface is kept.
func TestCallerReadScope_AsksOnTheTurnSurface(t *testing.T) {
	t.Parallel()
	var got SurfaceRef
	e := &ToolExecutor{authz: surfaceRecordingAuthz{got: &got}}
	tg := SurfaceRef{Kind: SurfaceChat, ID: "chat:telegram"}

	if eff, _ := e.callerReadScope(WithSurface(context.Background(), tg), ToolCallRequest{AgentID: "chat_agent"}); eff == nil {
		t.Fatal("no predicate")
	}
	if got != tg {
		t.Fatalf("ReadFilter surface = %+v, want the turn's %+v", got, tg)
	}
	e.callerReadScope(context.Background(), ToolCallRequest{AgentID: "chat_agent"})
	if got != (SurfaceRef{Kind: SurfaceAgent}) {
		t.Fatalf("no stamped surface: ReadFilter surface = %+v, want the agent default", got)
	}
}
