package operator

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/skill/access"
)

// fakeGrants stands in for *access.Service (its rules are tested in
// internal/skill/access); this holds the operator plane to its contract.
type fakeGrants struct {
	grants   []domain.SkillGrant
	requests []domain.SkillAccessRequest
	reqState string
	granted  []access.GrantInput
	revoked  []access.RevokeInput
	cmds     map[string]bool
	err      error
}

func (f *fakeGrants) Grants(context.Context, domain.SkillGrantFilter) ([]domain.SkillGrant, error) {
	return f.grants, nil
}
func (f *fakeGrants) Requests(_ context.Context, fl domain.SkillGrantFilter) ([]domain.SkillAccessRequest, error) {
	f.reqState = fl.State
	return f.requests, nil
}
func (f *fakeGrants) seen(cmd string) bool {
	if f.cmds == nil {
		f.cmds = map[string]bool{}
	}
	d := f.cmds[cmd]
	f.cmds[cmd] = true
	return d
}
func (f *fakeGrants) Grant(_ context.Context, in access.GrantInput) (access.Result, error) {
	if f.err != nil {
		return access.Result{}, f.err
	}
	f.granted = append(f.granted, in)
	return access.Result{Changed: true, Deduped: f.seen(in.CommandID)}, nil
}
func (f *fakeGrants) Revoke(_ context.Context, in access.RevokeInput) (access.Result, error) {
	if f.err != nil {
		return access.Result{}, f.err
	}
	f.revoked = append(f.revoked, in)
	return access.Result{Changed: true, Deduped: f.seen(in.CommandID)}, nil
}

func grantsService(f *fakeGrants) (*Service, *InMemoryAuditStore) {
	svc := NewService(NewSpool(SpoolConfig{}))
	audit := NewInMemoryAuditStore()
	svc.SetCommandSources(audit, domain.NewInMemoryGrantsStore())
	svc.SetSkillGrants(f)
	return svc, audit
}

func TestSkillGrantRPCs_UnimplementedWithoutStore(t *testing.T) {
	svc := NewService(NewSpool(SpoolConfig{}))
	svc.SetCommandSources(NewInMemoryAuditStore(), domain.NewInMemoryGrantsStore())
	ctx := operatorCtx()
	_, e1 := svc.ListSkillGrants(ctx, &pb.ListSkillGrantsOpRequest{})
	_, e2 := svc.GrantSkill(ctx, &pb.GrantSkillOpRequest{CommandId: "c", Reason: "r", AgentId: "a", SkillName: "s"})
	_, e3 := svc.RevokeSkillGrant(ctx, &pb.RevokeSkillGrantOpRequest{CommandId: "c", Reason: "r", AgentId: "a", SkillName: "s"})
	for i, err := range []error{e1, e2, e3} {
		if status.Code(err) != codes.Unimplemented {
			t.Errorf("rpc %d: %v", i, err)
		}
	}
	if svc.HasSkillGrants() {
		t.Fatal("capability gate reports wired")
	}
}

func TestSkillGrantRPCs_RoleGate(t *testing.T) {
	for _, m := range []string{"GrantSkill", "RevokeSkillGrant"} {
		if !isOperatorOnly(operatorMethodPrefix + m) {
			t.Errorf("%s must be operator-only", m)
		}
	}
	if isOperatorOnly(operatorMethodPrefix + "ListSkillGrants") {
		t.Error("ListSkillGrants is a read")
	}
}

func TestGrantSkill_AuditedDedupedAndValidated(t *testing.T) {
	f := &fakeGrants{}
	svc, audit := grantsService(f)
	req := &pb.GrantSkillOpRequest{CommandId: "g1", Reason: "needed for reports", AgentId: "analyst", SkillName: "proposed-x"}
	ack, err := svc.GrantSkill(operatorCtx(), req)
	if err != nil || ack.GetDeduped() {
		t.Fatalf("ack=%v err=%v", ack, err)
	}
	if len(f.granted) != 1 || f.granted[0].Source != domain.SkillGrantSourceOperator || f.granted[0].By != "alice" {
		t.Fatalf("granted = %+v", f.granted)
	}
	rows, _ := audit.Query(context.Background(), AuditFilter{CommandID: "g1"})
	if len(rows) != 1 || rows[0].ActionType != "grant_skill" || rows[0].TargetID != domain.SkillGrantID("analyst", "proposed-x") {
		t.Fatalf("audit = %+v", rows)
	}
	if ack, _ := svc.GrantSkill(operatorCtx(), req); !ack.GetDeduped() {
		t.Fatal("retry must dedupe")
	}
	if _, err := svc.GrantSkill(operatorCtx(), &pb.GrantSkillOpRequest{CommandId: "g2", AgentId: "a", SkillName: "s"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing reason: %v", err)
	}
	if _, err := svc.GrantSkill(operatorCtx(), &pb.GrantSkillOpRequest{CommandId: "g3", Reason: "r", SkillName: "s"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("missing agent: %v", err)
	}
	f.err = access.ErrFailedPrecondition
	if _, err := svc.GrantSkill(operatorCtx(), &pb.GrantSkillOpRequest{CommandId: "g4", Reason: "r", AgentId: "a", SkillName: "ghost"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unknown skill: %v", err)
	}
	if rows, _ := audit.Query(context.Background(), AuditFilter{CommandID: "g4"}); len(rows) != 0 {
		t.Fatal("a refused command must not be audited")
	}
}

func TestRevokeSkillGrant_AuditedAndNotFound(t *testing.T) {
	f := &fakeGrants{}
	svc, audit := grantsService(f)
	if _, err := svc.RevokeSkillGrant(operatorCtx(), &pb.RevokeSkillGrantOpRequest{CommandId: "r1", Reason: "done", AgentId: "analyst", SkillName: "proposed-x"}); err != nil {
		t.Fatal(err)
	}
	rows, _ := audit.Query(context.Background(), AuditFilter{CommandID: "r1"})
	if len(f.revoked) != 1 || len(rows) != 1 || rows[0].ActionType != "revoke_skill_grant" {
		t.Fatalf("revoked=%+v audit=%+v", f.revoked, rows)
	}
	f.err = access.ErrNotFound
	if _, err := svc.RevokeSkillGrant(operatorCtx(), &pb.RevokeSkillGrantOpRequest{CommandId: "r2", Reason: "x", AgentId: "a", SkillName: "s"}); status.Code(err) != codes.NotFound {
		t.Fatalf("no grant: %v", err)
	}
}

func TestListSkillGrants_MapsAndDefaultsToPendingRequests(t *testing.T) {
	f := &fakeGrants{
		grants:   []domain.SkillGrant{{ID: "sg-1", AgentID: "analyst", SkillName: "proposed-x", Source: "request", GrantedBy: "op"}},
		requests: []domain.SkillAccessRequest{{ID: "sar-1", AgentID: "analyst", SkillName: "other", State: "pending", RunID: "run-1"}},
	}
	svc, _ := grantsService(f)
	resp, err := svc.ListSkillGrants(operatorCtx(), &pb.ListSkillGrantsOpRequest{})
	if err != nil || len(resp.GetGrants()) != 1 || len(resp.GetRequests()) != 1 || f.reqState != "pending" {
		t.Fatalf("resp=%v err=%v state=%q", resp, err, f.reqState)
	}
	if g := resp.GetGrants()[0]; g.GetSource() != "request" || g.GetRevokedAt() != nil {
		t.Fatalf("grant = %v", g)
	}
	if _, err := svc.ListSkillGrants(operatorCtx(), &pb.ListSkillGrantsOpRequest{RequestState: "all"}); err != nil || f.reqState != "" {
		t.Fatalf("all: %v %q", err, f.reqState)
	}
	if _, err := svc.ListSkillGrants(operatorCtx(), &pb.ListSkillGrantsOpRequest{RequestState: "weird"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad state: %v", err)
	}
}
