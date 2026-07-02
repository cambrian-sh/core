package operator_test

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/cambrian-sh/cambrian-runtime/internal/substrate/operator"
)

func newIDP() *operator.StaticIdentity {
	return operator.NewStaticIdentity(map[string]struct {
		Password string
		Role     operator.Role
	}{
		"op":  {Password: "pw", Role: operator.RoleOperator},
		"viewer": {Password: "pw", Role: operator.RoleViewer},
	})
}

func ctxWithToken(token string) context.Context {
	md := metadata.Pairs("authorization", "Bearer "+token)
	return metadata.NewIncomingContext(context.Background(), md)
}

func login(t *testing.T, idp *operator.StaticIdentity, user, pw string) string {
	t.Helper()
	token, _, _, err := idp.Login(user, pw)
	if err != nil {
		t.Fatalf("login(%s): %v", user, err)
	}
	return token
}

func callUnary(idp operator.OperatorIdentity, ctx context.Context, method string) (called bool, err error) {
	interceptor := operator.UnaryAuthInterceptor(idp)
	_, err = interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: method},
		func(context.Context, any) (any, error) { called = true; return nil, nil })
	return called, err
}

// Login passes through unauthenticated.
func TestAuth_LoginIsUnauthenticatedPassthrough(t *testing.T) {
	called, err := callUnary(newIDP(), context.Background(), "/cambrian.OperatorConsole/Login")
	if err != nil || !called {
		t.Fatalf("Login should pass through unauthenticated: called=%v err=%v", called, err)
	}
}

// Agent-facing (non-OperatorConsole) RPCs are never gated.
func TestAuth_NonOperatorMethodPassesThrough(t *testing.T) {
	called, err := callUnary(newIDP(), context.Background(), "/cambrian.Orchestrator/Execute")
	if err != nil || !called {
		t.Fatalf("agent RPC must pass through ungated: called=%v err=%v", called, err)
	}
}

// An unauthenticated operator RPC is rejected.
func TestAuth_UnauthenticatedOperatorRPCRejected(t *testing.T) {
	_, err := callUnary(newIDP(), context.Background(), "/cambrian.OperatorConsole/Snapshot")
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", err)
	}
}

// A Viewer is denied a mutating command; an Operator is allowed.
func TestAuth_ViewerDeniedCommandOperatorAllowed(t *testing.T) {
	idp := newIDP()
	const cmd = "/cambrian.OperatorConsole/SetToolGrant"

	viewerTok := login(t, idp, "viewer", "pw")
	if _, err := callUnary(idp, ctxWithToken(viewerTok), cmd); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("viewer should be denied %s, got %v", cmd, err)
	}

	opTok := login(t, idp, "op", "pw")
	called, err := callUnary(idp, ctxWithToken(opTok), cmd)
	if err != nil || !called {
		t.Fatalf("operator should be allowed %s: called=%v err=%v", cmd, called, err)
	}
}

// A Viewer is allowed read RPCs (feed/snapshot).
func TestAuth_ViewerAllowedReads(t *testing.T) {
	idp := newIDP()
	tok := login(t, idp, "viewer", "pw")
	called, err := callUnary(idp, ctxWithToken(tok), "/cambrian.OperatorConsole/Snapshot")
	if err != nil || !called {
		t.Fatalf("viewer should be allowed Snapshot: called=%v err=%v", called, err)
	}
}

// The principal is resolved into the handler context (actor-from-identity).
func TestAuth_PrincipalInContext(t *testing.T) {
	idp := newIDP()
	tok := login(t, idp, "op", "pw")
	interceptor := operator.UnaryAuthInterceptor(idp)
	var gotPrincipal string
	var gotRole operator.Role
	_, err := interceptor(ctxWithToken(tok), nil, &grpc.UnaryServerInfo{FullMethod: "/cambrian.OperatorConsole/Snapshot"},
		func(ctx context.Context, _ any) (any, error) {
			gotPrincipal, gotRole, _ = operator.PrincipalFromContext(ctx)
			return nil, nil
		})
	if err != nil || gotPrincipal != "op" || gotRole != operator.RoleOperator {
		t.Fatalf("expected principal op/operator in ctx, got %q/%q err=%v", gotPrincipal, gotRole, err)
	}
}

// Bad credentials and bad tokens are rejected.
func TestAuth_StaticIdentityRejectsBadCreds(t *testing.T) {
	idp := newIDP()
	if _, _, _, err := idp.Login("op", "wrong"); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("bad password should be Unauthenticated, got %v", err)
	}
	if _, _, err := idp.Resolve("not-a-token"); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("bad token should be Unauthenticated, got %v", err)
	}
}
