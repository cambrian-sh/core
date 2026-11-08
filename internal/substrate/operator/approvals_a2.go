package operator

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// WatchToolApprovals streams pending approval requests to an operator console
// (ADR-0047 Amendment A2.5). It mirrors the ADR-0039 ApprovalRequest shape
// (tool_name / agent_id / args_preview) that the console's approval surfaces
// render — deliberately NOT HITLRaisedOp, which lacks those fields.
//
// Since ADR-0142 D11 it is ADDRESSED: it streams the pending items the caller
// may answer (for an operator, exactly the kinds that were always the operator's:
// tools, skill access, schedules, proactive plans), with the rule and the new
// ApprovalOp fields filled. It never streams resolutions — clients that predate
// ApprovalOp.state would read one as a new request; they learn of resolutions
// from the feed's HITLResolvedOp. The full audit view is WatchMyApprovals with
// audit=true.
//
// Resolution reuses ResolveHITL over the SAME hub id-space (the approval id ==
// the ResolveHITL intervention_id).
func (s *Service) WatchToolApprovals(_ *pb.SubscribeRequest, stream pb.OperatorConsole_WatchToolApprovalsServer) error {
	if s.hitl == nil {
		return status.Error(codes.Unimplemented, "operator approval hub not configured")
	}
	me, ok := callerApprover(stream.Context())
	if !ok {
		return status.Error(codes.Unauthenticated, "approvals require an authenticated caller")
	}
	ch, cancel := s.hitl.Watch(domain.ApprovalWatch{For: me})
	defer cancel()
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			if ev.Type != domain.ApprovalEventRaised {
				continue
			}
			if err := stream.Send(approvalOp(ev.Item, ev.Answerable)); err != nil {
				return err
			}
		}
	}
}

// callerApprover is the verified answerer of an operator-plane call: the
// authenticated person (kind user) and their full role set, plus the plane role
// the interceptor derived (so a console operator always holds "operator").
func callerApprover(ctx context.Context) (domain.Approver, bool) {
	principal, role, ok := PrincipalFromContext(ctx)
	if !ok || principal == "" {
		return domain.Approver{}, false
	}
	p := domain.PrincipalFromContext(ctx)
	if p.IsZero() {
		p = domain.UserPrincipal(principal)
	}
	roles := append([]string(nil), domain.PrincipalRolesFromContext(ctx)...)
	if role != "" && !contains(roles, string(role)) {
		roles = append(roles, string(role))
	}
	return domain.Approver{Principal: p, Roles: roles}, true
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// approvalOp renders a hub item on the operator plane.
func approvalOp(it domain.ApprovalItem, answerable bool) *pb.ApprovalOp {
	op := &pb.ApprovalOp{
		Id:          it.ID,
		AgentId:     it.AgentID,
		ToolName:    it.ToolName,
		ArgsPreview: it.ArgsPreview,
		// A tool approval is raised ONLY for a dangerous tool (ADR-0039), so it is
		// destructive by construction. A skill_access request widens what an agent
		// may read; a machine choice or an account confirmation changes nothing.
		IsDestructive: it.Kind != domain.ApprovalKindSkillAccess && it.Kind != domain.ApprovalKindChooseMachine &&
			it.Kind != domain.ApprovalKindAccountConfirm && it.Kind != domain.ApprovalKindPublish &&
			it.Kind != domain.ApprovalKindKnowledgeCommit,
		Kind:           it.Kind,
		Rule:           it.RuleSummary(),
		Answerable:     answerable,
		State:          string(it.State),
		ResolvedBy:     it.ResolvedBy,
		ActionClass:    it.ActionClass,
		AllowStanding:  it.AllowStanding,
		Object:         it.Object,
		ConversationId: it.ConversationID,
		Machine:        it.Machine,
		Candidates:     append([]string(nil), it.Candidates...),
		Detail:         it.Detail,
		SessionId:      it.SessionID,
		// What earlier call of the conversation this one may duplicate
		// (T-20261107-approver-is-not-told-that-an-identical-side).
		DuplicateNotice: it.DuplicateNotice,
	}
	if !it.Beneficiary.IsZero() {
		op.Beneficiary = it.Beneficiary.String()
	}
	if !it.RaisedAt.IsZero() {
		op.RaisedAtUnixMs = it.RaisedAt.UnixMilli()
	}
	if !it.ExpiresAt.IsZero() {
		op.ExpiresAtUnixMs = it.ExpiresAt.UnixMilli()
	}
	return op
}
