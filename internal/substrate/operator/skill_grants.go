package operator

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/skill/access"
)

// SkillGrantAdmin is per-agent skill access as the operator plane uses it
// (decisions/skill-access-grants.md). *access.Service satisfies it.
type SkillGrantAdmin interface {
	Grants(ctx context.Context, f domain.SkillGrantFilter) ([]domain.SkillGrant, error)
	Requests(ctx context.Context, f domain.SkillGrantFilter) ([]domain.SkillAccessRequest, error)
	Grant(ctx context.Context, in access.GrantInput) (access.Result, error)
	Revoke(ctx context.Context, in access.RevokeInput) (access.Result, error)
}

// SetSkillGrants wires the grant surface. The composition root calls it only when
// the durable grant store is ready, and advertises "skill-grants" iff it did.
func (s *Service) SetSkillGrants(g SkillGrantAdmin) { s.skillGrants = g }

// HasSkillGrants reports whether the grant surface is wired (the capability gate).
func (s *Service) HasSkillGrants() bool { return s.skillGrants != nil }

const skillGrantsUnimplemented = "this kernel has no skill grant store (capability skill-grants)"

func grantErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, access.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, access.ErrFailedPrecondition):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, access.ErrInvalidArgument):
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return status.Errorf(codes.Internal, "skill grants: %v", err)
}

// ListSkillGrants lists grants and access requests (default: pending requests).
func (s *Service) ListSkillGrants(ctx context.Context, req *pb.ListSkillGrantsOpRequest) (*pb.ListSkillGrantsOpResponse, error) {
	if s.skillGrants == nil {
		return nil, status.Error(codes.Unimplemented, skillGrantsUnimplemented)
	}
	f := domain.SkillGrantFilter{AgentID: req.GetAgentId(), SkillName: req.GetSkillName(), IncludeRevoked: req.GetIncludeRevoked()}
	grants, err := s.skillGrants.Grants(ctx, f)
	if err != nil {
		return nil, grantErr(err)
	}
	switch st := req.GetRequestState(); st {
	case "":
		f.State = domain.SkillAccessPending
	case "all":
		f.State = ""
	case domain.SkillAccessPending, domain.SkillAccessGranted, domain.SkillAccessDenied:
		f.State = st
	default:
		return nil, status.Errorf(codes.InvalidArgument, "request_state %q: want pending | granted | denied | all", st)
	}
	reqs, err := s.skillGrants.Requests(ctx, f)
	if err != nil {
		return nil, grantErr(err)
	}
	out := &pb.ListSkillGrantsOpResponse{}
	for _, g := range grants {
		out.Grants = append(out.Grants, &pb.SkillGrantOp{
			Id: g.ID, AgentId: g.AgentID, SkillName: g.SkillName, Source: g.Source, GrantedBy: g.GrantedBy,
			GrantedAt: ts(g.GrantedAt), Reason: g.Reason, RevokedAt: ts(g.RevokedAt), RevokedBy: g.RevokedBy,
			RevokeReason: g.RevokeReason,
		})
	}
	for _, r := range reqs {
		out.Requests = append(out.Requests, &pb.SkillAccessRequestOp{
			Id: r.ID, AgentId: r.AgentID, SkillName: r.SkillName, RunId: r.RunID, Rationale: r.Rationale,
			State: r.State, RequestedAt: ts(r.RequestedAt), DecidedAt: ts(r.DecidedAt), DecidedBy: r.DecidedBy,
		})
	}
	return out, nil
}

// grantCommand is the shared shape of GrantSkill / RevokeSkillGrant: validate,
// apply (idempotent on command_id in the grant audit itself), then write the
// operator audit row (idempotent on command_id), which also emits the AuditEvent.
// A refused command writes no audit row, so it can be retried once fixed.
func (s *Service) grantCommand(ctx context.Context, commandID, reason, action, agentID, skill string,
	apply func(actor string) (access.Result, error)) (*pb.CommandAck, error) {
	if s.skillGrants == nil {
		return nil, status.Error(codes.Unimplemented, skillGrantsUnimplemented)
	}
	if s.audit == nil {
		return nil, status.Error(codes.Unimplemented, "operator audit store not configured")
	}
	if commandID == "" || reason == "" {
		return nil, status.Error(codes.InvalidArgument, "command_id and reason are required")
	}
	if agentID == "" || skill == "" {
		return nil, status.Error(codes.InvalidArgument, "agent_id and skill_name are required")
	}
	actor, role, _ := PrincipalFromContext(ctx)
	res, err := apply(actor)
	if err != nil {
		return nil, grantErr(err)
	}
	after, _ := json.Marshal(map[string]any{"agent_id": agentID, "skill_name": skill, "changed": res.Changed})
	auditDeduped, err := s.recordAndEmit(ctx, domain.AuditEntry{
		ID: newAuditID(), CommandID: commandID, At: time.Now().UTC(),
		Actor: actor, Role: string(role), ActionType: action,
		TargetType: "skill_grant", TargetID: domain.SkillGrantID(agentID, skill), After: string(after), Reason: reason, Result: "ok",
	})
	if err != nil {
		return nil, err
	}
	return &pb.CommandAck{CommandId: commandID, Deduped: res.Deduped || auditDeduped}, nil
}

// GrantSkill grants a skill to an agent directly (source "operator"). Granting an
// active grant is a no-op that still acknowledges.
func (s *Service) GrantSkill(ctx context.Context, req *pb.GrantSkillOpRequest) (*pb.CommandAck, error) {
	return s.grantCommand(ctx, req.GetCommandId(), req.GetReason(), "grant_skill", req.GetAgentId(), req.GetSkillName(),
		func(actor string) (access.Result, error) {
			return s.skillGrants.Grant(ctx, access.GrantInput{
				CommandID: req.GetCommandId(), AgentID: req.GetAgentId(), Skill: req.GetSkillName(),
				Source: domain.SkillGrantSourceOperator, By: actor, Reason: req.GetReason(),
			})
		})
}

// RevokeSkillGrant ends an agent's active grant of a skill.
func (s *Service) RevokeSkillGrant(ctx context.Context, req *pb.RevokeSkillGrantOpRequest) (*pb.CommandAck, error) {
	return s.grantCommand(ctx, req.GetCommandId(), req.GetReason(), "revoke_skill_grant", req.GetAgentId(), req.GetSkillName(),
		func(actor string) (access.Result, error) {
			return s.skillGrants.Revoke(ctx, access.RevokeInput{
				CommandID: req.GetCommandId(), AgentID: req.GetAgentId(), Skill: req.GetSkillName(),
				By: actor, Reason: req.GetReason(),
			})
		})
}
