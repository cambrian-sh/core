package operator

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// SetSteeringSources wires the live-execution control hub and the HITL approval
// hub used by the steering commands (ADR-0047 D11).
func (s *Service) SetSteeringSources(hub *ExecutionControlHub, hitl domain.ApprovalHub) {
	s.controls = hub
	s.hitl = hitl
}

// ResolveHITL approves or rejects a raised HITL intervention, reusing the
// kernel ApprovalHub (ADR-0047 D11). Idempotent against the intervention id via
// command_id; audited.
func (s *Service) ResolveHITL(ctx context.Context, req *pb.ResolveHITLRequest) (*pb.CommandAck, error) {
	if s.hitl == nil {
		return nil, status.Error(codes.Unimplemented, "operator HITL hub not configured")
	}
	if req.GetReason() == "" || req.GetCommandId() == "" || req.GetInterventionId() == "" {
		return nil, status.Error(codes.InvalidArgument, "command_id, reason and intervention_id are required")
	}
	actor, role, _ := PrincipalFromContext(ctx)

	deduped, err := s.recordAndEmit(ctx, domain.AuditEntry{
		ID: newAuditID(), CommandID: req.GetCommandId(), At: time.Now().UTC(),
		Actor: actor, Role: string(role), ActionType: "resolve_hitl",
		TargetType: "intervention", TargetID: req.GetInterventionId(),
		After: boolStr(req.GetApprove()), Reason: req.GetReason(), Result: "ok",
	})
	if err != nil {
		return nil, err
	}
	if !deduped {
		s.hitl.Submit(req.GetInterventionId(), req.GetApprove(), actor)
	}
	return &pb.CommandAck{CommandId: req.GetCommandId(), Deduped: deduped}, nil
}

// PauseSession pauses a live execution via the control hub (ADR-0047 D11).
func (s *Service) PauseSession(ctx context.Context, req *pb.SessionCommandRequest) (*pb.CommandAck, error) {
	return s.steer(ctx, req, "pause_session", func(c ExecutionControls) { c.Pause() })
}

// ResumeSession resumes a paused execution via the control hub.
func (s *Service) ResumeSession(ctx context.Context, req *pb.SessionCommandRequest) (*pb.CommandAck, error) {
	return s.steer(ctx, req, "resume_session", func(c ExecutionControls) { c.Resume() })
}

// steer is the shared body for the session control commands: validate, resolve
// the live execution, audit, and apply once (idempotent).
func (s *Service) steer(ctx context.Context, req *pb.SessionCommandRequest, action string, apply func(ExecutionControls)) (*pb.CommandAck, error) {
	if s.controls == nil {
		return nil, status.Error(codes.Unimplemented, "operator control hub not configured")
	}
	if req.GetReason() == "" || req.GetCommandId() == "" || req.GetSessionId() == "" {
		return nil, status.Error(codes.InvalidArgument, "command_id, reason and session_id are required")
	}
	controls, ok := s.controls.Lookup(req.GetSessionId())
	if !ok {
		return nil, status.Errorf(codes.FailedPrecondition, "no live execution for session %s", req.GetSessionId())
	}
	actor, role, _ := PrincipalFromContext(ctx)

	deduped, err := s.recordAndEmit(ctx, domain.AuditEntry{
		ID: newAuditID(), CommandID: req.GetCommandId(), At: time.Now().UTC(),
		Actor: actor, Role: string(role), ActionType: action,
		TargetType: "session", TargetID: req.GetSessionId(),
		Reason: req.GetReason(), Result: "ok",
	})
	if err != nil {
		return nil, err
	}
	if !deduped {
		apply(controls)
	}
	return &pb.CommandAck{CommandId: req.GetCommandId(), Deduped: deduped}, nil
}

func boolStr(b bool) string {
	if b {
		return "approved"
	}
	return "rejected"
}
