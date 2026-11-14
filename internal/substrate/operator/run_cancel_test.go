package operator_test

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/identity"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// Contract 0102 (T-20261106-operator-closesession-only-pauses-a-live-plan):
// CancelRun ends a tracked run terminally; CloseSession only pauses.

func TestTrackRun_CancelRunCancelsBySessionAndByRunID(t *testing.T) {
	t.Parallel()
	hub := operator.NewExecutionControlHub()
	ctxA, doneA := hub.TrackRun(context.Background(), "s1")
	defer doneA()
	operator.AttachRunID(ctxA, "run-a")
	ctxB, doneB := hub.TrackRun(context.Background(), "s1")
	defer doneB()
	operator.AttachRunID(ctxB, "run-b")
	ctxC, doneC := hub.TrackRun(context.Background(), "s2")
	defer doneC()

	cause := &operator.RunCancelledError{Actor: "op", Reason: "wrong customer"}
	if n := hub.CancelRun("", "run-b", cause); n != 1 {
		t.Fatalf("cancel by run id hit %d, want 1", n)
	}
	if ctxA.Err() != nil || ctxB.Err() == nil {
		t.Fatalf("by run id: a=%v b=%v", ctxA.Err(), ctxB.Err())
	}
	rc, ok := operator.RunCancelled(ctxB)
	if !ok || rc.Actor != "op" || rc.Reason != "wrong customer" || !errors.Is(context.Cause(ctxB), operator.ErrRunCancelled) {
		t.Fatalf("cause = %v, %v", rc, ok)
	}
	if n := hub.CancelRun("s1", "", cause); n != 2 { // a and the already-cancelled b
		t.Fatalf("cancel by session hit %d, want 2", n)
	}
	if ctxA.Err() == nil || ctxC.Err() != nil {
		t.Fatalf("by session: a=%v c=%v", ctxA.Err(), ctxC.Err())
	}
	if n := hub.CancelRun("", "", cause); n != 0 {
		t.Fatal("an empty selector must match nothing")
	}
	// A finished run is untracked: its done() removes it.
	doneC()
	if n := hub.CancelRun("s2", "", cause); n != 0 {
		t.Fatalf("a finished run was still cancellable (%d)", n)
	}
	// A run ended by its own completion is not "cancelled".
	if _, ok := operator.RunCancelled(ctxC); ok {
		t.Fatal("a completed run reads as operator-cancelled")
	}
}

func TestCancelRun_StopsTheLiveRunAuditsAndDedups(t *testing.T) {
	t.Parallel()
	svc, _, _, _ := newCommandService()
	hub := operator.NewExecutionControlHub()
	svc.SetSteeringSources(hub, &fakeApprovalHub{})
	runCtx, done := hub.TrackRun(context.Background(), "s1")
	defer done()
	operator.AttachRunID(runCtx, "r1")

	req := &pb.CancelRunRequest{CommandId: "c1", Reason: "runaway spend", SessionId: "s1", RunId: "r1"}
	if _, err := svc.CancelRun(opCtx(), req); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	rc, ok := operator.RunCancelled(runCtx)
	if !ok || rc.Reason != "runaway spend" {
		t.Fatalf("run not cancelled with the reason: %v %v", rc, ok)
	}
	// A retry with the same command id while another run of s1 is live is
	// acknowledged as deduped and cancels nothing more.
	other, done2 := hub.TrackRun(context.Background(), "s1")
	defer done2()
	operator.AttachRunID(other, "r1")
	ack, err := svc.CancelRun(opCtx(), req)
	if err != nil || !ack.GetDeduped() {
		t.Fatalf("retry: ack=%+v err=%v", ack, err)
	}
	if other.Err() != nil {
		t.Fatal("a deduped retry cancelled another run")
	}
}

func TestCancelRun_RefusesBadRequestsAndNothingLive(t *testing.T) {
	t.Parallel()
	svc, _, _, _ := newCommandService()
	svc.SetSteeringSources(operator.NewExecutionControlHub(), &fakeApprovalHub{})
	for name, req := range map[string]*pb.CancelRunRequest{
		"no reason":   {CommandId: "c", SessionId: "s1"},
		"no command":  {Reason: "r", SessionId: "s1"},
		"no selector": {CommandId: "c", Reason: "r"},
	} {
		if _, err := svc.CancelRun(opCtx(), req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	_, err := svc.CancelRun(opCtx(), &pb.CancelRunRequest{CommandId: "c", Reason: "r", SessionId: "ghost"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("nothing live: %v", err)
	}
}

func TestCancelRun_IsOperatorOnlyAndAdvertised(t *testing.T) {
	t.Parallel()
	idp := newIDP()
	const m = "/cambrian.OperatorConsole/CancelRun"
	if _, err := callUnary(idp, ctxWithToken(login(t, idp, "viewer", "pw")), m); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("viewer must be denied CancelRun: %v", err)
	}
	if called, err := callUnary(idp, ctxWithToken(login(t, idp, "op", "pw")), m); err != nil || !called {
		t.Fatalf("operator must reach CancelRun: %v", err)
	}
	svc, _, _, _ := newCommandService()
	svc.SetSteeringSources(operator.NewExecutionControlHub(), &fakeApprovalHub{})
	found := false
	for _, c := range svc.Wave1Capabilities() {
		found = found || c == "run-cancel"
	}
	if !found {
		t.Fatal("run-cancel not advertised with a control hub")
	}
}

// WhoAmI (contract 0102, T-20261106-operator-plane-has-no-whoami-an-oidc-signed-in).
func TestWhoAmI_AnswersEveryRoleWithTheKernelsView(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reg := identity.NewMemRegistry()
	idp := identity.NewLocalAccounts(reg, reg)
	for u, r := range map[string][]string{"op": {"operator", "role:compliance"}, "viewer": {"viewer"}, "mem": nil} {
		if _, err := idp.EnsureAccount(ctx, u, "pw", r); err != nil {
			t.Fatal(err)
		}
	}
	svc := operator.NewService(operator.NewSpool(operator.SpoolConfig{}))
	svc.SetIdentity(idp)
	svc.SetPrincipalNames(reg)

	interceptor := operator.UnaryAuthInterceptor(idp, nil, nil)
	call := func(user string) (*pb.WhoAmIResponse, error) {
		tok := login(t, idp, user, "pw")
		out, err := interceptor(ctxWithToken(tok), &pb.WhoAmIRequest{},
			&grpc.UnaryServerInfo{FullMethod: "/cambrian.OperatorConsole/WhoAmI"},
			func(c context.Context, req any) (any, error) { return svc.WhoAmI(c, req.(*pb.WhoAmIRequest)) })
		if err != nil {
			return nil, err
		}
		return out.(*pb.WhoAmIResponse), nil
	}
	for user, want := range map[string]string{"op": "operator", "viewer": "viewer", "mem": "member"} {
		got, err := call(user)
		if err != nil {
			t.Fatalf("%s: %v", user, err)
		}
		if got.GetRole() != want || got.GetRoleSource() != "kernel" || got.GetPrincipal() == "" || got.GetIssuer() != "local" {
			t.Fatalf("%s: %+v", user, got)
		}
	}
	got, _ := call("op")
	if len(got.GetRoles()) != 2 || got.GetRoles()[1] != "role:compliance" {
		t.Fatalf("full role set lost: %v", got.GetRoles())
	}
	if p, err := reg.GetPrincipal(ctx, principalIDOf(t, got.GetPrincipal())); err == nil && got.GetDisplayName() != p.DisplayName {
		t.Fatalf("display name %q, registry %q", got.GetDisplayName(), p.DisplayName)
	}
}

func TestWhoAmI_RefusesAnUnknownBearer(t *testing.T) {
	t.Parallel()
	svc := operator.NewService(operator.NewSpool(operator.SpoolConfig{}))
	svc.SetIdentity(newIDP())
	if _, err := svc.WhoAmI(ctxWithToken("nope"), &pb.WhoAmIRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unknown bearer: %v", err)
	}
	if _, err := operator.NewService(operator.NewSpool(operator.SpoolConfig{})).WhoAmI(context.Background(), &pb.WhoAmIRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("no identity: %v", err)
	}
}

// principalIDOf strips the "user:" rendering back to the registry id.
func principalIDOf(t *testing.T, rendered string) string {
	t.Helper()
	ref := domain.PrincipalRef{}
	for i := 0; i < len(rendered); i++ {
		if rendered[i] == ':' {
			ref.ID = rendered[i+1:]
			return ref.ID
		}
	}
	return rendered
}
