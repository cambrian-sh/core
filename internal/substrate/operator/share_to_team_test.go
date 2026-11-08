package operator_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// fakeSharer: doc "d1" is bob's private document; "team_kb" is writable.
type fakeSharer struct{ applied map[string][]string }

func (f *fakeSharer) PlanShare(_ context.Context, p domain.PrincipalRef, doc string, target []string) ([]string, error) {
	if doc != "d1" || p.ID != "bob" {
		return nil, operator.ErrShareNotOwner
	}
	for _, t := range target {
		if t != "team_kb" {
			return nil, operator.ErrShareNotPermitted
		}
	}
	return target, nil
}

func (f *fakeSharer) ApplyShare(ctx context.Context, p domain.PrincipalRef, doc string, target []string) ([]string, error) {
	tags, err := f.PlanShare(ctx, p, doc, target)
	if err == nil {
		f.applied[doc] = tags
	}
	return tags, err
}

// ADR-0142 D12: sharing is explicit, owner-only, policy-checked and audited;
// under the default "publish: self" policy the owner's request applies at once.
func TestShareToTeam_OwnerSharesAuditedOthersRefused(t *testing.T) {
	t.Parallel()
	svc, hub, audit := hubService(t)
	sh := &fakeSharer{applied: map[string][]string{}}
	svc.SetMemorySharer(sh)
	hub.HandleKind(domain.ApprovalKindPublish, func(ctx context.Context, it domain.ApprovalItem) error {
		_, err := sh.ApplyShare(ctx, it.Beneficiary, it.Object, operator.ShareTargetsOf(it.ArgsPreview))
		return err
	})

	if _, err := svc.ShareToTeam(personCtx("carol", operator.RoleMember), &pb.ShareToTeamOpRequest{CommandId: "s0", Reason: "x", DocumentId: "d1", TargetTags: []string{"team_kb"}}); status.Code(err) != codes.NotFound {
		t.Fatalf("a non-owner must not share (NotFound, no existence leak), got %v", err)
	}
	if _, err := svc.ShareToTeam(personCtx("bob", operator.RoleMember), &pb.ShareToTeamOpRequest{CommandId: "s1", Reason: "x", DocumentId: "d1", TargetTags: []string{"secrets"}}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a target outside what bob may write must be refused, got %v", err)
	}
	if len(sh.applied) != 0 {
		t.Fatal("a refused share was applied")
	}
	resp, err := svc.ShareToTeam(personCtx("bob", operator.RoleMember), &pb.ShareToTeamOpRequest{CommandId: "s2", Reason: "team needs it", DocumentId: "d1", TargetTags: []string{"team_kb"}})
	if err != nil || resp.GetState() != "shared" || resp.GetApprovalId() == "" {
		t.Fatalf("owner share: %+v %v", resp, err)
	}
	if got := sh.applied["d1"]; len(got) != 1 || got[0] != "team_kb" {
		t.Fatalf("applied = %v", sh.applied)
	}
	rows, _ := audit.Query(context.Background(), operator.AuditFilter{})
	if len(rows) != 1 || rows[0].ActionType != "share_to_team" || rows[0].Actor != "bob" {
		t.Fatalf("the share must be audited once: %+v", rows)
	}
	it, _ := hub.Get(context.Background(), resp.GetApprovalId())
	if it.State != domain.ApprovalApproved || it.Kind != domain.ApprovalKindPublish {
		t.Fatalf("the publish approval record = %+v", it)
	}
}
