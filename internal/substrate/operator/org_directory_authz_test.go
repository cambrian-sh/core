package operator_test

import (
	"context"
	"testing"

	"google.golang.org/grpc"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/substrate/operator"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ADR-0142 stage 2: the premium OrgDirectoryAdmin plane. Every signed-in
// person reads their OWN reporting chain; the hierarchy reads are Viewer-open;
// mutations and the SCIM / reconcile reads are Operator-only.
func TestAuth_OrgDirectoryPlaneRoles(t *testing.T) {
	t.Parallel()
	idp := newIDP()
	if _, err := idp.EnsureAccount(context.Background(), "member", "pw", nil); err != nil {
		t.Fatal(err)
	}
	const svc = "/cambrian.premium.org.OrgDirectoryAdmin/"
	member, viewer, op := login(t, idp, "member", "pw"), login(t, idp, "viewer", "pw"), login(t, idp, "op", "pw")
	cases := []struct {
		tok, method string
		want        codes.Code
	}{
		{member, "GetMyOrgChain", codes.OK},
		{member, "GetOrgChain", codes.PermissionDenied},
		{member, "ListOrgPeople", codes.PermissionDenied},
		{viewer, "GetMyOrgChain", codes.OK},
		{viewer, "ListOrgUnits", codes.OK},
		{viewer, "PreviewMappingRules", codes.PermissionDenied},
		{viewer, "SaveOrgUnit", codes.PermissionDenied},
		{viewer, "ListScimConnections", codes.PermissionDenied},
		{viewer, "GetReconcileStatus", codes.PermissionDenied},
		{op, "SaveMappingRule", codes.OK},
		{op, "ListScimConnections", codes.OK},
		{op, "CreateScimConnection", codes.OK},
	}
	for _, c := range cases {
		_, err := callUnaryRoles(idp, ctxWithToken(c.tok), svc+c.method, operator.PremiumRolesFixture)
		if got := status.Code(err); got != c.want {
			t.Errorf("%s as %s: %v, want %v", c.method, c.tok[:4], got, c.want)
		}
	}
}

// callUnaryRoles is callUnary with the premium plugins' declared method roles
// (the org-directory plugin declares GetMyOrgChain self-service and the SCIM /
// reconcile reads operator-only; core names none of them).
func callUnaryRoles(idp domain.Authenticator, ctx context.Context, method string, roles operator.MethodRoles) (called bool, err error) {
	interceptor := operator.UnaryAuthInterceptor(idp, nil, roles)
	_, err = interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: method},
		func(context.Context, any) (any, error) { called = true; return nil, nil })
	return called, err
}
