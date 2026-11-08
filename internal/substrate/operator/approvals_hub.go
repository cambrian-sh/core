package operator

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// The ADR-0142 D11 addressed-approval RPCs (capability "addressed-approvals").
// Every one acts as the AUTHENTICATED caller (callerApprover); none takes a
// principal from the request. The hub authorises every answer at answer time —
// these handlers only scope reads to the caller and audit the mutations.

const defaultApprovalListLimit = 200

func (s *Service) approvalCaller(ctx context.Context, audit bool) (domain.Approver, error) {
	if s.hitl == nil {
		return domain.Approver{}, status.Error(codes.Unimplemented, "approval hub not configured")
	}
	me, ok := callerApprover(ctx)
	if !ok {
		return domain.Approver{}, status.Error(codes.Unauthenticated, "approvals require an authenticated caller")
	}
	if audit && !me.HasRole(domain.ApprovalRoleOperator) {
		return domain.Approver{}, status.Error(codes.PermissionDenied, "the audit view is operator-only")
	}
	return me, nil
}

// WatchMyApprovals streams the approvals addressed to the caller — raised and
// resolved — or, with audit (operator role), every approval marked answerable
// or not.
func (s *Service) WatchMyApprovals(req *pb.WatchMyApprovalsOpRequest, stream pb.OperatorConsole_WatchMyApprovalsServer) error {
	me, err := s.approvalCaller(stream.Context(), req.GetAudit())
	if err != nil {
		return err
	}
	ch, cancel := s.hitl.Watch(domain.ApprovalWatch{For: me, All: req.GetAudit(), Resolutions: true})
	defer cancel()
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			if ev.Type == domain.ApprovalEventNotice {
				continue
			}
			answerable := ev.Answerable
			if ev.Type == domain.ApprovalEventResolved {
				answerable = false
			}
			if err := stream.Send(approvalOp(ev.Item, answerable)); err != nil {
				return err
			}
		}
	}
}

// ListApprovals returns the items the caller may answer (or all, audit).
func (s *Service) ListApprovals(ctx context.Context, req *pb.ListApprovalsOpRequest) (*pb.ListApprovalsOpResponse, error) {
	me, err := s.approvalCaller(ctx, req.GetAudit())
	if err != nil {
		return nil, err
	}
	q := domain.ApprovalQuery{}
	for _, st := range req.GetStates() {
		q.States = append(q.States, domain.ApprovalState(st))
	}
	if len(q.States) == 0 {
		q.States = []domain.ApprovalState{domain.ApprovalPending}
	}
	limit := int(req.GetLimit())
	if limit <= 0 || limit > defaultApprovalListLimit {
		limit = defaultApprovalListLimit
	}
	items, err := s.hitl.List(ctx, q)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "list approvals: %v", err)
	}
	resp := &pb.ListApprovalsOpResponse{}
	for _, it := range items {
		ok, _ := s.hitl.Answerable(ctx, it, me)
		if !ok && !req.GetAudit() {
			continue
		}
		resp.Approvals = append(resp.Approvals, approvalOp(it, ok && it.State == domain.ApprovalPending))
		if len(resp.Approvals) >= limit {
			break
		}
	}
	return resp, nil
}

// AnswerApproval answers one approval as the caller. A refusal is ok=false with
// the reason, never a silent success. Audited (command_id + reason) once the
// hub has authorised the answer.
func (s *Service) AnswerApproval(ctx context.Context, req *pb.AnswerApprovalOpRequest) (*pb.AnswerApprovalOpResponse, error) {
	me, err := s.approvalCaller(ctx, false)
	if err != nil {
		return nil, err
	}
	if req.GetCommandId() == "" || req.GetReason() == "" || req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "command_id, reason and id are required")
	}
	resp := &pb.AnswerApprovalOpResponse{CommandId: req.GetCommandId()}
	it, err := s.hitl.Get(ctx, req.GetId())
	if errors.Is(err, domain.ErrApprovalNotFound) {
		resp.Refusal = "unknown approval"
		return resp, nil
	}
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "approval lookup: %v", err)
	}
	if it.State == domain.ApprovalPending {
		if ok, why := s.hitl.Answerable(ctx, it, me); !ok {
			resp.State, resp.Refusal = string(it.State), why
			return resp, nil
		}
	}
	after := "denied"
	if req.GetApprove() {
		after = "approved"
		if req.GetStanding() {
			after = "approved+standing"
		}
	}
	actor, role, _ := PrincipalFromContext(ctx)
	deduped, err := s.recordAndEmit(ctx, domain.AuditEntry{
		ID: newAuditID(), CommandID: req.GetCommandId(), At: time.Now().UTC(),
		Actor: actor, Role: string(role), ActionType: "answer_approval",
		TargetType: "approval", TargetID: req.GetId(), After: after,
		Reason: req.GetReason(), Result: "ok",
	})
	if err != nil {
		return nil, err
	}
	resp.Deduped = deduped
	if deduped {
		cur, _ := s.hitl.Get(ctx, req.GetId())
		resp.Ok, resp.State, resp.StandingId = true, string(cur.State), cur.StandingID
		return resp, nil
	}
	res, err := s.hitl.Answer(ctx, req.GetId(), domain.ApprovalAnswer{
		Approved:    req.GetApprove(),
		Choice:      req.GetChoice(),
		By:          me,
		Standing:    req.GetStanding(),
		StandingTTL: time.Duration(req.GetStandingTtlSeconds()) * time.Second,
		Reason:      req.GetReason(),
	})
	resp.State, resp.StandingId = string(res.State), res.StandingID
	if err != nil {
		resp.Refusal = err.Error()
		return resp, nil
	}
	resp.Ok = true
	return resp, nil
}

// ListStandingApprovals lists the caller's standing approvals (as beneficiary or
// granter), or every one with audit (operator role).
func (s *Service) ListStandingApprovals(ctx context.Context, req *pb.ListStandingApprovalsOpRequest) (*pb.ListStandingApprovalsOpResponse, error) {
	me, err := s.approvalCaller(ctx, req.GetAudit())
	if err != nil {
		return nil, err
	}
	list, err := s.hitl.ListStanding(ctx, domain.StandingQuery{IncludeInactive: req.GetIncludeInactive()})
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "list standing approvals: %v", err)
	}
	now := time.Now()
	resp := &pb.ListStandingApprovalsOpResponse{}
	for _, st := range list {
		mine := domain.SamePerson(me.Principal, st.Beneficiary) || domain.SamePerson(me.Principal, st.GrantedBy.Principal)
		if !mine && !req.GetAudit() {
			continue
		}
		resp.Standing = append(resp.Standing, standingOp(st, now))
	}
	return resp, nil
}

// RevokeStandingApproval revokes one standing approval (beneficiary, granter or
// operator — the hub decides). Audited after the hub accepted it.
func (s *Service) RevokeStandingApproval(ctx context.Context, req *pb.RevokeStandingApprovalOpRequest) (*pb.CommandAck, error) {
	me, err := s.approvalCaller(ctx, false)
	if err != nil {
		return nil, err
	}
	if req.GetCommandId() == "" || req.GetReason() == "" || req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "command_id, reason and id are required")
	}
	if _, err := s.hitl.RevokeStanding(ctx, req.GetId(), me, req.GetReason()); err != nil {
		switch {
		case errors.Is(err, domain.ErrStandingNotFound):
			return nil, status.Error(codes.NotFound, "unknown standing approval")
		case errors.Is(err, domain.ErrApprovalNotAddressee):
			return nil, status.Error(codes.PermissionDenied, err.Error())
		}
		return nil, status.Errorf(codes.Unavailable, "revoke standing approval: %v", err)
	}
	actor, role, _ := PrincipalFromContext(ctx)
	deduped, err := s.recordAndEmit(ctx, domain.AuditEntry{
		ID: newAuditID(), CommandID: req.GetCommandId(), At: time.Now().UTC(),
		Actor: actor, Role: string(role), ActionType: "revoke_standing_approval",
		TargetType: "standing_approval", TargetID: req.GetId(), After: "revoked",
		Reason: req.GetReason(), Result: "ok",
	})
	if err != nil {
		return nil, err
	}
	return &pb.CommandAck{CommandId: req.GetCommandId(), Deduped: deduped}, nil
}

func standingOp(st domain.StandingApproval, now time.Time) *pb.StandingApprovalOp {
	ms := func(t time.Time) int64 {
		if t.IsZero() {
			return 0
		}
		return t.UnixMilli()
	}
	op := &pb.StandingApprovalOp{
		Id: st.ID, ActionClass: st.ActionClass, Resource: st.Resource, Scope: st.Scope,
		GrantedBy: st.GrantedBy.Principal.String(), GrantedAtUnixMs: ms(st.GrantedAt),
		ExpiresAtUnixMs: ms(st.ExpiresAt), RevokedAtUnixMs: ms(st.RevokedAt), RevokedBy: st.RevokedBy,
		Reason: st.Reason, LastUsedAtUnixMs: ms(st.LastUsedAt), UseCount: int32(st.UseCount),
		FromApproval: st.FromApproval, Active: st.Active(now),
	}
	if !st.Beneficiary.IsZero() {
		op.Beneficiary = st.Beneficiary.String()
	}
	return op
}
