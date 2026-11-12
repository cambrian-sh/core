package operator_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The premium Ingress Studio plane's ReplayDeadDeliveries (owner decision
// 2026-11-13, T-20261113-dead-evidence-outbox-items-have-no-replay-path) is
// fenced like the plane's other write RPCs (PromoteRepair, Pause): Operator
// only. Nothing names it in authz.go — the fail-closed naming rule (anything
// not Query*/List*/Get*/Describe* is a command) is what covers it, and this
// test is what notices if that ever stops being true. Its read neighbour
// ListDeliveryFailures stays Viewer-open.
func TestAuth_IngressStudioReplayDeadIsOperatorOnly(t *testing.T) {
	t.Parallel()
	idp := newIDP()
	if _, err := idp.EnsureAccount(context.Background(), "member", "pw", nil); err != nil {
		t.Fatal(err)
	}
	const svc = "/cambrian.premium.ingress.IngressStudioAdmin/"
	member, viewer, op := login(t, idp, "member", "pw"), login(t, idp, "viewer", "pw"), login(t, idp, "op", "pw")
	cases := []struct {
		who, tok, method string
		want             codes.Code
	}{
		{"anonymous", "", "ReplayDeadDeliveries", codes.Unauthenticated},
		{"member", member, "ReplayDeadDeliveries", codes.PermissionDenied},
		{"viewer", viewer, "ReplayDeadDeliveries", codes.PermissionDenied},
		{"viewer", viewer, "PromoteRepair", codes.PermissionDenied},
		{"viewer", viewer, "ListDeliveryFailures", codes.OK},
		{"operator", op, "ReplayDeadDeliveries", codes.OK},
	}
	for _, c := range cases {
		ctx := context.Background()
		if c.tok != "" {
			ctx = ctxWithToken(c.tok)
		}
		called, err := callUnary(idp, ctx, svc+c.method)
		if got := status.Code(err); got != c.want {
			t.Errorf("%s as %s: %v, want %v", c.method, c.who, got, c.want)
		}
		if called != (c.want == codes.OK) {
			t.Errorf("%s as %s: handler called=%v", c.method, c.who, called)
		}
	}
}
