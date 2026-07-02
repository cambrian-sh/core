package operator

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// SessionOps is the chat-and-steer seam (ADR-0047 0047-10): create a session,
// send a message into it, and inject a mid-plan correction. The kernel adapter
// (SessionOpsFuncs) binds these to the SessionManager and the Execute path;
// nil hooks return Unimplemented (honest where dispatch is not yet wired).
type SessionOps interface {
	Create(ctx context.Context, goal, parentID string) (sessionID string, err error)
	SendMessage(ctx context.Context, sessionID, text string) error
}

// SessionOpsFuncs adapts plain functions to SessionOps so the composition root
// can bind kernel handles without the operator package importing them. A nil
// function yields Unimplemented.
type SessionOpsFuncs struct {
	CreateFn func(ctx context.Context, goal, parentID string) (string, error)
	SendFn   func(ctx context.Context, sessionID, text string) error
}

func (f SessionOpsFuncs) Create(ctx context.Context, goal, parentID string) (string, error) {
	if f.CreateFn == nil {
		return "", status.Error(codes.Unimplemented, "create session not wired")
	}
	return f.CreateFn(ctx, goal, parentID)
}

func (f SessionOpsFuncs) SendMessage(ctx context.Context, sessionID, text string) error {
	if f.SendFn == nil {
		return status.Error(codes.Unimplemented, "send message not wired")
	}
	return f.SendFn(ctx, sessionID, text)
}

// SetSessionOps wires the chat-and-steer adapter.
func (s *Service) SetSessionOps(ops SessionOps) { s.sessionOps = ops }

// CreateSession creates a new session and returns its id. Audited; idempotent on
// command_id (a retry returns deduped without creating a second session).
func (s *Service) CreateSession(ctx context.Context, req *pb.CreateSessionRequest) (*pb.CreateSessionResponse, error) {
	if s.sessionOps == nil {
		return nil, status.Error(codes.Unimplemented, "session ops not configured")
	}
	if req.GetCommandId() == "" || req.GetReason() == "" {
		return nil, status.Error(codes.InvalidArgument, "command_id and reason are required")
	}
	actor, role, _ := PrincipalFromContext(ctx)

	// The audit Record is the dedup gate; only create on a new command_id.
	deduped, err := s.recordAndEmit(ctx, domain.AuditEntry{
		ID: newAuditID(), CommandID: req.GetCommandId(), At: time.Now().UTC(),
		Actor: actor, Role: string(role), ActionType: "create_session",
		TargetType: "session", After: req.GetGoal(), Reason: req.GetReason(), Result: "ok",
	})
	if err != nil {
		return nil, err
	}
	if deduped {
		return &pb.CreateSessionResponse{CommandId: req.GetCommandId(), Deduped: true}, nil
	}
	sessionID, err := s.sessionOps.Create(ctx, req.GetGoal(), req.GetParentId())
	if err != nil {
		return nil, err
	}
	return &pb.CreateSessionResponse{CommandId: req.GetCommandId(), SessionId: sessionID}, nil
}

// SendMessage sends a natural-language message into a session.
func (s *Service) SendMessage(ctx context.Context, req *pb.SendMessageRequest) (*pb.CommandAck, error) {
	if s.sessionOps == nil {
		return nil, status.Error(codes.Unimplemented, "session ops not configured")
	}
	return s.runMutation(ctx, req.GetCommandId(), req.GetReason(), "send_message", "session", req.GetSessionId(),
		req.GetText(), func() error { return s.sessionOps.SendMessage(ctx, req.GetSessionId(), req.GetText()) })
}

// InjectCorrection delivers an instruction into a running plan (ADR-0047 A1.1):
// it reaches the live execution via the control hub; the executor owns the
// pause→replan→hot-swap→resume mechanism and the Planner does the routing
// (Zero-Hardcode, D16). A session with no live execution returns FailedPrecondition.
func (s *Service) InjectCorrection(ctx context.Context, req *pb.InjectCorrectionRequest) (*pb.CommandAck, error) {
	if s.controls == nil {
		return nil, status.Error(codes.Unimplemented, "operator control hub not configured")
	}
	if req.GetInstruction() == "" {
		return nil, status.Error(codes.InvalidArgument, "instruction is required")
	}
	controls, ok := s.controls.Lookup(req.GetSessionId())
	if !ok {
		return nil, status.Errorf(codes.FailedPrecondition, "no live execution for session %s", req.GetSessionId())
	}
	return s.runMutation(ctx, req.GetCommandId(), req.GetReason(), "inject_correction", "session", req.GetSessionId(),
		req.GetInstruction(), func() error { return controls.Inject(req.GetInstruction()) })
}
