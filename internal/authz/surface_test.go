package authz_test

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// The surface follows the SERVICE the call was routed to, which is a property of
// the connection rather than of anything the caller wrote in the message.
func TestSurfaceForMethod(t *testing.T) {
	cases := []struct {
		method   string
		wantKind string
	}{
		{"/cambrian.OperatorConsole/Snapshot", domain.SurfaceOperator},
		{"/cambrian.OperatorConsole/ExplainAccess", domain.SurfaceOperator},
		{"/cambrian.Orchestrator/QueryMemory", domain.SurfaceAgent},
		{"/cambrian.Orchestrator/ExecuteTool", domain.SurfaceAgent},
		// A premium plane is mounted behind the SAME operator interceptors, so it is
		// the operator plane extended — not a new privilege level.
		{"/cambrian.premium.authz.AccessPolicyAdmin/SavePolicy", domain.SurfaceOperator},
		{"/cambrian.premium.reactive.ReactiveControl/EmitSignal", domain.SurfaceOperator},
		{"/grpc.health.v1.Health/Check", domain.SurfaceInternal},
		{"", domain.SurfaceInternal},
	}
	for _, tc := range cases {
		if got := authz.SurfaceForMethod(tc.method); got.Kind != tc.wantKind {
			t.Errorf("SurfaceForMethod(%q).Kind = %q, want %q", tc.method, got.Kind, tc.wantKind)
		}
	}
}

// INV-5: the surface is established by the KERNEL from the transport, and a
// caller-supplied header claiming to be someone else's surface changes nothing.
// A black box asserting its own privilege level is not a security boundary.
func TestSurfaceInterceptor_IgnoresCallerSuppliedClaims(t *testing.T) {
	var seen domain.SurfaceRef
	handler := func(ctx context.Context, _ any) (any, error) {
		seen = domain.SurfaceFromContext(ctx)
		return nil, nil
	}
	// The caller tries to claim the operator surface over the AGENT plane.
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"x-surface", "operator",
		"x-surface-kind", "operator",
		"surface", "operator",
	))
	interceptor := authz.UnarySurfaceInterceptor()
	if _, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/cambrian.Orchestrator/QueryMemory"}, handler); err != nil {
		t.Fatal(err)
	}
	if seen.Kind != domain.SurfaceAgent {
		t.Fatalf("surface = %q, want %q — a header must never be able to name the surface",
			seen.Kind, domain.SurfaceAgent)
	}
}

// The interceptor runs unconditionally: the kernel always establishes WHERE a
// request came from, whether or not any policy cares.
func TestSurfaceInterceptor_AlwaysStamps(t *testing.T) {
	var seen domain.SurfaceRef
	handler := func(ctx context.Context, _ any) (any, error) {
		seen = domain.SurfaceFromContext(ctx)
		return nil, nil
	}
	interceptor := authz.UnarySurfaceInterceptor()
	if _, err := interceptor(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/cambrian.OperatorConsole/Snapshot"}, handler); err != nil {
		t.Fatal(err)
	}
	if seen.Kind != domain.SurfaceOperator || seen.ID == "" {
		t.Fatalf("expected a fully-identified operator surface, got %+v", seen)
	}
}

// fakeSessions returns a recorded surface for one session id.
type fakeSessions struct {
	id      domain.SessionID
	surface domain.SurfaceRef
}

func (f fakeSessions) SessionSurface(_ context.Context, id domain.SessionID) (domain.SurfaceRef, bool) {
	if id != f.id {
		return domain.SurfaceRef{}, false
	}
	return f.surface, true
}

// A session's RECORDED surface wins over the transport-derived one. This is what
// keeps a conversation clamped across turns: an outsider conversation stays an
// outsider conversation even when a later turn arrives over an internal path.
// Widening on the way in is exactly the escalation the clamp prevents.
func TestResolveSurface_SessionOverridesTransport(t *testing.T) {
	sessions := fakeSessions{id: "sess-1", surface: domain.SurfaceRef{Kind: domain.SurfaceChat, ID: "chat:public"}}

	// Arriving on the INTERNAL agent plane, but under an outsider session.
	ctx := domain.WithSurface(context.Background(), domain.SurfaceRef{Kind: domain.SurfaceAgent, ID: "grpc"})
	ctx = domain.WithSessionID(ctx, "sess-1")

	got := authz.ResolveSurface(ctx, sessions)
	if got.Kind != domain.SurfaceChat || got.ID != "chat:public" {
		t.Fatalf("the session's surface must win, got %+v", got)
	}
}

// Without a session — or with one that recorded no surface — the transport's
// answer stands. There is nothing narrower to defer to.
func TestResolveSurface_FallsBackToTransport(t *testing.T) {
	transport := domain.SurfaceRef{Kind: domain.SurfaceAgent, ID: "grpc"}
	ctx := domain.WithSurface(context.Background(), transport)

	if got := authz.ResolveSurface(ctx, nil); got != transport {
		t.Errorf("with no session reader, the transport surface stands, got %+v", got)
	}
	if got := authz.ResolveSurface(ctx, fakeSessions{id: "other"}); got != transport {
		t.Errorf("an unmatched session must not blank the surface, got %+v", got)
	}
	unrecorded := fakeSessions{id: "sess-1", surface: domain.SurfaceRef{}}
	ctx = domain.WithSessionID(ctx, "sess-1")
	if got := authz.ResolveSurface(ctx, unrecorded); got != transport {
		t.Errorf("a session with no recorded surface must fall back, got %+v", got)
	}
}
