package operator

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
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

// PauseSession pauses a live execution via the control hub AND persists the session's
// status (ADR-0047 D11 + Phase 2).
func (s *Service) PauseSession(ctx context.Context, req *pb.SessionCommandRequest) (*pb.CommandAck, error) {
	return s.steer(ctx, req, "pause_session", domain.SessionPaused, func(c ExecutionControls) { c.Pause() })
}

// ResumeSession resumes a paused execution via the control hub AND persists the status.
func (s *Service) ResumeSession(ctx context.Context, req *pb.SessionCommandRequest) (*pb.CommandAck, error) {
	return s.steer(ctx, req, "resume_session", domain.SessionActive, func(c ExecutionControls) { c.Resume() })
}

// CloseSession seals a session (→ completed). Unlike pause/resume it does NOT require a
// live execution: closing is a lifecycle act on the session record, and the common case is
// sealing work that has already stopped running. That asymmetry is deliberate — requiring a
// live executor here would make a finished session impossible to close, which is exactly
// how the lifecycle came to have no terminator.
func (s *Service) CloseSession(ctx context.Context, req *pb.SessionCommandRequest) (*pb.CommandAck, error) {
	if s.sessionOps == nil {
		return nil, status.Error(codes.Unimplemented, "session ops not configured")
	}
	if req.GetReason() == "" || req.GetCommandId() == "" || req.GetSessionId() == "" {
		return nil, status.Error(codes.InvalidArgument, "command_id, reason and session_id are required")
	}
	actor, role, _ := PrincipalFromContext(ctx)
	deduped, err := s.recordAndEmit(ctx, domain.AuditEntry{
		ID: newAuditID(), CommandID: req.GetCommandId(), At: time.Now().UTC(),
		Actor: actor, Role: string(role), ActionType: "close_session",
		TargetType: "session", TargetID: req.GetSessionId(),
		After: string(domain.SessionCompleted), Reason: req.GetReason(), Result: "ok",
	})
	if err != nil {
		return nil, err
	}
	if !deduped {
		// Best-effort control-hub stop: a session being closed mid-run should not keep
		// executing. A session with no live execution closes normally.
		if s.controls != nil {
			if c, ok := s.controls.Lookup(req.GetSessionId()); ok {
				c.Pause()
			}
		}
		if err := s.sessionOps.SetStatus(ctx, req.GetSessionId(), domain.SessionCompleted, req.GetReason()); err != nil {
			return nil, status.Errorf(codes.FailedPrecondition, "close session: %v", err)
		}
	}
	return &pb.CommandAck{CommandId: req.GetCommandId(), Deduped: deduped}, nil
}

// steer is the shared body for the session control commands: validate, resolve
// the live execution, audit, and apply once (idempotent).
func (s *Service) steer(ctx context.Context, req *pb.SessionCommandRequest, action string, persist domain.SessionStatus, apply func(ExecutionControls)) (*pb.CommandAck, error) {
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
		// Phase 2: persist the lifecycle status so the console (and any consumer of the
		// feed) sees the state the operator just commanded. Steering the in-memory
		// executor without this left the durable status stuck on "active".
		if s.sessionOps != nil {
			if err := s.sessionOps.SetStatus(ctx, req.GetSessionId(), persist, req.GetReason()); err != nil {
				// The executor has already been steered; report the partial outcome
				// rather than pretending the command fully applied.
				return nil, status.Errorf(codes.Internal, "%s: execution steered but status not persisted: %v", action, err)
			}
		}
	}
	return &pb.CommandAck{CommandId: req.GetCommandId(), Deduped: deduped}, nil
}

func boolStr(b bool) string {
	if b {
		return "approved"
	}
	return "rejected"
}
