package operator_test

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/identity"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// ADR-0142 D1/D3 on the console door: the domain principal is the registry
// person (kind user), roles come from the person's role SET, a person with no
// console role is a member confined to their own surfaces, and a suspended
// person is refused on the next call.

func TestRoleFromRoles(t *testing.T) {
	for roles, want := range map[string]operator.Role{
		"operator": operator.RoleOperator, "admin": operator.RoleOperator, "viewer": operator.RoleViewer,
		"": operator.RoleMember, "compliance": operator.RoleMember,
	} {
		if got := operator.RoleFromRoles([]string{roles}); got != want {
			t.Errorf("RoleFromRoles(%q) = %q, want %q", roles, got, want)
		}
	}
	if operator.RoleFromRoles([]string{"viewer", "operator"}) != operator.RoleOperator {
		t.Error("operator must win over viewer")
	}
}

func principalVia(t *testing.T, idp domain.Authenticator, token, method string) (domain.PrincipalRef, error) {
	t.Helper()
	var got domain.PrincipalRef
	_, err := operator.UnaryAuthInterceptor(idp, nil)(ctxWithToken(token), nil,
		&grpc.UnaryServerInfo{FullMethod: method},
		func(ctx context.Context, _ any) (any, error) { got = domain.PrincipalFromContext(ctx); return nil, nil })
	return got, err
}

func TestConsoleDoor_PersonIsUserAndMemberIsConfined(t *testing.T) {
	ctx := context.Background()
	reg := identity.NewMemRegistry()
	la := identity.NewLocalAccounts(reg, reg)
	if _, err := la.EnsureAccount(ctx, "maria", "pw", nil); err != nil { // no console role
		t.Fatal(err)
	}
	tok, id, err := la.Login(ctx, "maria", "pw")
	if err != nil || id.Principal != domain.UserPrincipal("maria") {
		t.Fatalf("login: %+v %v", id, err)
	}
	// Her own conversation lane: allowed, as user:maria.
	p, err := principalVia(t, la, tok, "/cambrian.OperatorConsole/SendTurn")
	if err != nil || p != domain.UserPrincipal("maria") {
		t.Fatalf("member SendTurn: %v %v", p, err)
	}
	// Anything that reads or changes the deployment: refused, reads included.
	for _, m := range []string{"/cambrian.OperatorConsole/Snapshot", "/cambrian.OperatorConsole/QueryAudit",
		"/cambrian.OperatorConsole/StreamEvents", "/cambrian.premium.reactive.ReactiveControl/EmitSignal"} {
		if _, err := principalVia(t, la, tok, m); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("member %s: %v, want PermissionDenied", m, err)
		}
	}
	// Suspension takes effect on the next call.
	_ = reg.SetStatus(ctx, "maria", domain.PrincipalSuspended)
	if _, err := principalVia(t, la, tok, "/cambrian.OperatorConsole/SendTurn"); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("suspended: %v", err)
	}
}

func TestDescribeAuth_IsUnauthenticatedAndHonest(t *testing.T) {
	reg := identity.NewMemRegistry()
	la := identity.NewLocalAccounts(reg, reg)
	called := false
	_, err := operator.UnaryAuthInterceptor(la, nil)(context.Background(), nil,
		&grpc.UnaryServerInfo{FullMethod: "/cambrian.OperatorConsole/DescribeAuth"},
		func(context.Context, any) (any, error) { called = true; return nil, nil })
	if err != nil || !called {
		t.Fatalf("DescribeAuth must be reachable without a token: %v", err)
	}
	svc := operator.NewService(operator.NewSpool(operator.SpoolConfig{}))
	svc.SetIdentity(identity.NewChain(la, nil))
	resp, err := svc.DescribeAuth(context.Background(), &pb.DescribeAuthRequest{})
	if err != nil || !resp.GetPasswordLogin() || resp.GetOidcIssuer() != "" {
		t.Fatalf("DescribeAuth = %+v %v", resp, err)
	}
}

// An OIDC-only authenticator has no password path: Login says so instead of
// pretending a password could work.
func TestLogin_RefusedWithoutPasswordCapability(t *testing.T) {
	svc := operator.NewService(operator.NewSpool(operator.SpoolConfig{}))
	svc.SetIdentity(tokenOnly{})
	if _, err := svc.Login(context.Background(), &pb.LoginRequest{Username: "a", Password: "b"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Login on a token-only kernel: %v", err)
	}
}

type tokenOnly struct{}

func (tokenOnly) Resolve(context.Context, string) (domain.Identity, error) {
	return domain.Identity{}, domain.ErrUnauthenticated
}
func (tokenOnly) Revoke(context.Context, string) error { return nil }
