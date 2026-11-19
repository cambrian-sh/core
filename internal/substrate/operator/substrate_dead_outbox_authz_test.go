package operator_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The premium SubstrateLane's dead evidence-outbox surface (owner decision
// 2026-11-19, T-20261113-dead-outbox-items-of-non-ingress-sources-have,
// ENG-1100): ReplayDeadOutbox re-runs work and is Operator only, ListDeadOutbox
// is a read open to a Viewer. Nothing names either in authz.go — the fail-closed
// naming rule covers them, and this test notices if that ever stops being true.
func TestAuth_SubstrateReplayDeadOutboxIsOperatorOnly(t *testing.T) {
	t.Parallel()
	idp := newIDP()
	if _, err := idp.EnsureAccount(context.Background(), "member", "pw", nil); err != nil {
		t.Fatal(err)
	}
	const svc = "/cambrian.premium.substrate.SubstrateLane/"
	member, viewer, op := login(t, idp, "member", "pw"), login(t, idp, "viewer", "pw"), login(t, idp, "op", "pw")
	cases := []struct {
		who, tok, method string
		want             codes.Code
	}{
		{"anonymous", "", "ReplayDeadOutbox", codes.Unauthenticated},
		{"member", member, "ReplayDeadOutbox", codes.PermissionDenied},
		{"viewer", viewer, "ReplayDeadOutbox", codes.PermissionDenied},
		{"viewer", viewer, "ListDeadOutbox", codes.OK},
		{"operator", op, "ReplayDeadOutbox", codes.OK},
		{"operator", op, "ListDeadOutbox", codes.OK},
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
