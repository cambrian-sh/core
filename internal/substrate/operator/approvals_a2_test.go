package operator_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

type fakeApprovalStream struct {
	grpc.ServerStream
	ctx  context.Context
	recv chan *pb.ApprovalOp
}

func (f *fakeApprovalStream) Send(a *pb.ApprovalOp) error { f.recv <- a; return nil }
func (f *fakeApprovalStream) Context() context.Context    { return f.ctx }

// hubService wires a Service over a real addressed hub (ADR-0142 D11).
func hubService(t *testing.T) (*operator.Service, *domain.AddressedHub, *operator.InMemoryAuditStore) {
	t.Helper()
	hub := domain.NewAddressedHub(domain.AddressedHubConfig{DefaultTimeout: 5 * time.Second, ConsentTimeout: 5 * time.Second})
	audit := operator.NewInMemoryAuditStore()
	svc := operator.NewService(operator.NewSpool(operator.SpoolConfig{}))
	svc.SetCommandSources(audit, domain.NewInMemoryGrantsStore())
	svc.SetSteeringSources(operator.NewExecutionControlHub(), hub)
	return svc, hub, audit
}

func personCtx(id string, role operator.Role) context.Context {
	ctx := operator.ContextWithPrincipal(context.Background(), id, role)
	return domain.WithPrincipal(ctx, domain.UserPrincipal(id))
}

// raiseUntilStreamed raises fresh requests until the (asynchronously
// subscribed) stream yields one, returning what it yielded.
func raiseUntilStreamed(t *testing.T, hub *domain.AddressedHub, stream *fakeApprovalStream, req domain.ApprovalRequest) *pb.ApprovalOp {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for i := 0; ; i++ {
		r := req
		r.ArgsPreview = req.ArgsPreview + string(rune('a'+i%26))
		if _, err := hub.Raise(context.Background(), r); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-stream.recv:
			return got
		case <-time.After(30 * time.Millisecond):
		case <-deadline:
			t.Fatal("timed out waiting for an ApprovalOp")
		}
	}
}

// WatchToolApprovals streams the ADR-0039 approval shape (tool_name / agent_id /
// args_preview) plus the ADR-0142 rule, and ResolveHITL resolves it over the
// SAME hub id-space — the intervention_id IS the approval id (A2.5).
func TestWatchToolApprovals_StreamsAndSharesApprovalHubIDSpace(t *testing.T) {
	t.Parallel()
	svc, hub, _ := hubService(t)
	ctx, cancel := context.WithCancel(opCtx())
	defer cancel()
	stream := &fakeApprovalStream{ctx: ctx, recv: make(chan *pb.ApprovalOp, 64)}
	go func() { _ = svc.WatchToolApprovals(&pb.SubscribeRequest{}, stream) }()

	got := raiseUntilStreamed(t, hub, stream, domain.ApprovalRequest{AgentID: "agent-9", ToolName: "shell_exec", ArgsPreview: `{"cmd":"rm -rf"}`})
	if got.GetId() == "" || got.GetToolName() != "shell_exec" || !got.GetIsDestructive() || got.GetRule() != "operator" ||
		!got.GetAnswerable() || got.GetState() != "pending" {
		t.Fatalf("ApprovalOp mapping wrong: %+v", got)
	}
	if _, err := svc.ResolveHITL(opCtx(), &pb.ResolveHITLRequest{
		CommandId: "r1", Reason: "operator denies", InterventionId: got.GetId(), Approve: false,
	}); err != nil {
		t.Fatalf("ResolveHITL: %v", err)
	}
	it, _ := hub.Get(context.Background(), got.GetId())
	if it.State != domain.ApprovalDenied || it.ResolvedBy != "alice" {
		t.Fatalf("ResolveHITL must resolve the same hub id: %+v", it)
	}
}

func TestWatchToolApprovals_Unconfigured(t *testing.T) {
	t.Parallel()
	svc := operator.NewService(operator.NewSpool(operator.SpoolConfig{}))
	stream := &fakeApprovalStream{ctx: opCtx(), recv: make(chan *pb.ApprovalOp, 1)}
	if err := svc.WatchToolApprovals(&pb.SubscribeRequest{}, stream); err == nil {
		t.Fatal("expected Unimplemented when the approval hub is not wired")
	}
}

// A skill_access request rides the same stream with its kind, and is not
// destructive: it widens what an agent may read.
func TestWatchToolApprovals_CarriesSkillAccessKind(t *testing.T) {
	t.Parallel()
	svc, hub, _ := hubService(t)
	ctx, cancel := context.WithCancel(opCtx())
	defer cancel()
	stream := &fakeApprovalStream{ctx: ctx, recv: make(chan *pb.ApprovalOp, 64)}
	go func() { _ = svc.WatchToolApprovals(&pb.SubscribeRequest{}, stream) }()
	got := raiseUntilStreamed(t, hub, stream, domain.ApprovalRequest{AgentID: "analyst", ToolName: "skill:proposed-x", Kind: domain.ApprovalKindSkillAccess})
	if got.GetKind() != domain.ApprovalKindSkillAccess || got.GetIsDestructive() || got.GetToolName() != "skill:proposed-x" {
		t.Fatalf("ApprovalOp = %+v", got)
	}
}

// ADR-0142 D11: an operator is NOT an addressee of a self-ruled approval.
// ResolveHITL and AnswerApproval both refuse it, nothing is audited as
// resolved, and the beneficiary answers it through AnswerApproval.
func TestApprovals_OnlyAddresseesAnswer_OperatorRefused(t *testing.T) {
	t.Parallel()
	svc, hub, audit := hubService(t)
	it, err := hub.Raise(context.Background(), domain.ApprovalRequest{
		Kind: domain.ApprovalKindMachineConsent, ToolName: "local:laptop/write_file", Machine: "laptop",
		Beneficiary: domain.UserPrincipal("bob"),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.ResolveHITL(opCtx(), &pb.ResolveHITLRequest{CommandId: "c1", Reason: "r", InterventionId: it.ID, Approve: true})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("an operator answering a self-ruled approval must be PermissionDenied, got %v", err)
	}
	resp, err := svc.AnswerApproval(opCtx(), &pb.AnswerApprovalOpRequest{CommandId: "c2", Reason: "r", Id: it.ID, Approve: true})
	if err != nil || resp.GetOk() || resp.GetRefusal() == "" {
		t.Fatalf("AnswerApproval by a non-addressee must refuse: %+v %v", resp, err)
	}
	if rows, _ := audit.Query(context.Background(), operator.AuditFilter{}); len(rows) != 0 {
		t.Fatalf("a refused answer must not be audited as resolved: %+v", rows)
	}
	// Carol (a member) is not Bob either.
	if resp, _ := svc.AnswerApproval(personCtx("carol", operator.RoleMember), &pb.AnswerApprovalOpRequest{CommandId: "c3", Reason: "r", Id: it.ID, Approve: true}); resp.GetOk() {
		t.Fatal("another person answered bob's approval")
	}
	resp, err = svc.AnswerApproval(personCtx("bob", operator.RoleMember), &pb.AnswerApprovalOpRequest{CommandId: "c4", Reason: "mine", Id: it.ID, Approve: true})
	if err != nil || !resp.GetOk() || resp.GetState() != "approved" {
		t.Fatalf("the beneficiary must be able to answer: %+v %v", resp, err)
	}
	// Retrying the same command is deduped, not a second answer.
	resp, err = svc.AnswerApproval(personCtx("bob", operator.RoleMember), &pb.AnswerApprovalOpRequest{CommandId: "c4", Reason: "mine", Id: it.ID, Approve: true})
	if err != nil || !resp.GetDeduped() || !resp.GetOk() {
		t.Fatalf("retry must be deduped: %+v %v", resp, err)
	}
}

// ListApprovals shows a person their own addressed items only; the audit view
// is operator-only and shows everything, marked answerable or not.
func TestListApprovals_AddressedAndAudit(t *testing.T) {
	t.Parallel()
	svc, hub, _ := hubService(t)
	_, _ = hub.Raise(context.Background(), domain.ApprovalRequest{Kind: domain.ApprovalKindAccountConfirm, ToolName: "gmail", Beneficiary: domain.UserPrincipal("bob")})
	_, _ = hub.Raise(context.Background(), domain.ApprovalRequest{AgentID: "a", ToolName: "shell"})

	bob, err := svc.ListApprovals(personCtx("bob", operator.RoleMember), &pb.ListApprovalsOpRequest{})
	if err != nil || len(bob.GetApprovals()) != 1 || bob.GetApprovals()[0].GetKind() != domain.ApprovalKindAccountConfirm || !bob.GetApprovals()[0].GetAnswerable() {
		t.Fatalf("bob's inbox = %+v %v", bob.GetApprovals(), err)
	}
	if _, err := svc.ListApprovals(personCtx("bob", operator.RoleMember), &pb.ListApprovalsOpRequest{Audit: true}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("the audit view must be operator-only, got %v", err)
	}
	all, err := svc.ListApprovals(opCtx(), &pb.ListApprovalsOpRequest{Audit: true})
	if err != nil || len(all.GetApprovals()) != 2 {
		t.Fatalf("operator audit view = %+v %v", all.GetApprovals(), err)
	}
	for _, a := range all.GetApprovals() {
		if a.GetKind() == domain.ApprovalKindAccountConfirm && a.GetAnswerable() {
			t.Fatal("the audit view marked bob's self-ruled item answerable by an operator")
		}
	}
}

// Standing approvals: granted through AnswerApproval, listed for the
// beneficiary, revoked by them (audited), and a stranger cannot revoke.
func TestStandingApprovals_GrantListRevoke(t *testing.T) {
	t.Parallel()
	svc, hub, audit := hubService(t)
	it, _ := hub.Raise(context.Background(), domain.ApprovalRequest{
		Kind: domain.ApprovalKindMachineConsent, ToolName: "local:laptop/write_file", Machine: "laptop",
		Beneficiary: domain.UserPrincipal("bob"),
	})
	resp, err := svc.AnswerApproval(personCtx("bob", operator.RoleMember), &pb.AnswerApprovalOpRequest{
		CommandId: "g1", Reason: "always fine", Id: it.ID, Approve: true, Standing: true, StandingTtlSeconds: 3600,
	})
	if err != nil || !resp.GetOk() || resp.GetStandingId() == "" {
		t.Fatalf("standing grant: %+v %v", resp, err)
	}
	list, err := svc.ListStandingApprovals(personCtx("bob", operator.RoleMember), &pb.ListStandingApprovalsOpRequest{})
	if err != nil || len(list.GetStanding()) != 1 || !list.GetStanding()[0].GetActive() || list.GetStanding()[0].GetExpiresAtUnixMs() == 0 {
		t.Fatalf("bob's standing approvals = %+v %v", list.GetStanding(), err)
	}
	if other, _ := svc.ListStandingApprovals(personCtx("carol", operator.RoleMember), &pb.ListStandingApprovalsOpRequest{}); len(other.GetStanding()) != 0 {
		t.Fatal("carol sees bob's standing approvals")
	}
	if _, err := svc.RevokeStandingApproval(personCtx("carol", operator.RoleMember), &pb.RevokeStandingApprovalOpRequest{CommandId: "r0", Reason: "x", Id: resp.GetStandingId()}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a stranger's revoke must be PermissionDenied, got %v", err)
	}
	if _, err := svc.RevokeStandingApproval(personCtx("bob", operator.RoleMember), &pb.RevokeStandingApprovalOpRequest{CommandId: "r1", Reason: "no longer", Id: resp.GetStandingId()}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	list, _ = svc.ListStandingApprovals(personCtx("bob", operator.RoleMember), &pb.ListStandingApprovalsOpRequest{})
	if len(list.GetStanding()) != 0 {
		t.Fatalf("revoked grant still active: %+v", list.GetStanding())
	}
	rows, _ := audit.Query(context.Background(), operator.AuditFilter{})
	var actions []string
	for _, r := range rows {
		actions = append(actions, r.ActionType)
	}
	if len(rows) != 2 {
		t.Fatalf("expected answer + revoke audited, got %v", actions)
	}
}

// WatchMyApprovals streams raised AND resolved items for the caller.
func TestWatchMyApprovals_StreamsResolutions(t *testing.T) {
	t.Parallel()
	svc, hub, _ := hubService(t)
	ctx, cancel := context.WithCancel(personCtx("bob", operator.RoleMember))
	defer cancel()
	stream := &fakeApprovalStream{ctx: ctx, recv: make(chan *pb.ApprovalOp, 64)}
	go func() { _ = svc.WatchMyApprovals(&pb.WatchMyApprovalsOpRequest{}, stream) }()
	got := raiseUntilStreamed(t, hub, stream, domain.ApprovalRequest{Kind: domain.ApprovalKindAccountConfirm, ToolName: "gmail", Beneficiary: domain.UserPrincipal("bob")})
	if got.GetState() != "pending" || got.GetRule() != "self" || got.GetBeneficiary() != "user:bob" {
		t.Fatalf("raised op = %+v", got)
	}
	if _, err := hub.Answer(context.Background(), got.GetId(), domain.ApprovalAnswer{Approved: false, By: domain.Approver{Principal: domain.UserPrincipal("bob")}}); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case op := <-stream.recv:
			if op.GetId() == got.GetId() && op.GetState() == "denied" {
				return
			}
		case <-time.After(3 * time.Second):
			t.Fatal("no resolution streamed")
		}
	}
}
