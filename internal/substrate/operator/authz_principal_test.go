package operator_test

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// failClosedAuthorize is what a premium deployment's PDP does with an
// unidentified caller: refuse. OSS fails OPEN on the same input, which is
// exactly why this defect was invisible to the test suite — every OSS test
// passed while the documented operator path was broken in every deployment that
// actually enforces access control. So the check has to be written the closed
// way here, or the regression walks straight back in.
func failClosedAuthorize(ctx context.Context) error {
	if domain.PrincipalFromContext(ctx).IsZero() {
		return errors.New("denied: no principal")
	}
	return nil
}

// The interceptor must set the DOMAIN principal, not only its own private one.
//
// The assertion is deliberately made INSIDE the handler. A test that only
// checked the RPC returned OK would have passed throughout the defect: the
// interceptor resolved the caller, stashed it in an operator-plane private
// value, and the domain never saw it — so every operator RPC reached the kernel
// as `principal:<none>` one stack frame after authenticating.
func TestAuth_HandlerSeesTheLoggedInPrincipal(t *testing.T) {
	idp := newIDP()
	token := login(t, idp, "op", "pw")

	var got domain.PrincipalRef
	interceptor := operator.UnaryAuthInterceptor(idp, nil)
	if _, err := interceptor(ctxWithToken(token), nil,
		&grpc.UnaryServerInfo{FullMethod: "/cambrian.OperatorConsole/IngestMemoryOp"},
		func(ctx context.Context, _ any) (any, error) {
			got = domain.PrincipalFromContext(ctx)
			return nil, nil
		}); err != nil {
		t.Fatalf("an authenticated operator RPC must be admitted: %v", err)
	}

	if got.IsZero() {
		t.Fatal("the handler saw no principal: every write it performs reaches the authorization chokepoint anonymous")
	}
	if got.ID != "op" || got.Kind != domain.PrincipalUser {
		t.Fatalf("the handler must see the LOGGED-IN user, got %+v", got)
	}
}

// The whole point, stated as the deployment that breaks: a handler that
// authorizes fail-closed must be reachable by an authenticated operator with no
// per-call-site principal wrapping.
func TestAuth_FailClosedHandlerAdmitsAnAuthenticatedOperator(t *testing.T) {
	idp := newIDP()
	token := login(t, idp, "op", "pw")

	interceptor := operator.UnaryAuthInterceptor(idp, nil)
	_, err := interceptor(ctxWithToken(token), nil,
		&grpc.UnaryServerInfo{FullMethod: "/cambrian.OperatorConsole/IngestMemoryOp"},
		func(ctx context.Context, _ any) (any, error) { return nil, failClosedAuthorize(ctx) })
	if err != nil {
		t.Fatalf("a fail-closed handler must admit an authenticated operator: %v", err)
	}

	// And the contrast, so the test above cannot pass merely because the
	// authorizer is permissive: the same check on an unstamped context denies.
	if failClosedAuthorize(context.Background()) == nil {
		t.Fatal("the fail-closed check must deny an anonymous context, or it proves nothing")
	}
}

// Streams carry it too. StreamEvents runs for the whole life of a console
// session, so a stream handler reading an anonymous principal is the same defect
// with a longer blast radius.
func TestAuth_StreamHandlerSeesTheLoggedInPrincipal(t *testing.T) {
	idp := newIDP()
	token := login(t, idp, "op", "pw")

	var got domain.PrincipalRef
	interceptor := operator.StreamAuthInterceptor(idp, nil)
	err := interceptor(nil, stubStream{ctx: ctxWithToken(token)},
		&grpc.StreamServerInfo{FullMethod: "/cambrian.OperatorConsole/StreamEvents"},
		func(_ any, ss grpc.ServerStream) error {
			got = domain.PrincipalFromContext(ss.Context())
			return nil
		})
	if err != nil {
		t.Fatalf("an authenticated stream must be admitted: %v", err)
	}
	if got.ID != "op" || got.Kind != domain.PrincipalUser {
		t.Fatalf("the stream handler must see the logged-in user, got %+v", got)
	}
}

type stubStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s stubStream) Context() context.Context { return s.ctx }
