package operator

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// CancelRun ends a live run terminally (contract 0102; owner decision 2026-11-14,
// T-20261106-operator-closesession-only-pauses-a-live-plan).
//
// CloseSession only pauses a live execution; this cancels the context every
// matching Execute runs under (ExecutionControlHub.TrackRun), on both lanes. The
// cause carries the actor and the reason, so the run's owner records it as
// cancelled — not resumable — and the result the caller gets names who stopped it.
// Writes already dispatched are not rolled back: cancellation stops what has not
// happened yet, it does not undo what has.
//
// Validation, liveness, audit, then cancel — the same order as the other steering
// commands (steer): a command for nothing live fails FailedPrecondition and is not
// audited as applied; a retried command_id is acknowledged as deduped.
func (s *Service) CancelRun(ctx context.Context, req *pb.CancelRunRequest) (*pb.CommandAck, error) {
	if s.controls == nil {
		return nil, status.Error(codes.Unimplemented, "operator control hub not configured")
	}
	sessionID, runID := strings.TrimSpace(req.GetSessionId()), strings.TrimSpace(req.GetRunId())
	if strings.TrimSpace(req.GetReason()) == "" || req.GetCommandId() == "" || (sessionID == "" && runID == "") {
		return nil, status.Error(codes.InvalidArgument, "command_id, reason and a session_id or run_id are required")
	}
	if s.controls.liveRuns(sessionID, runID) == 0 {
		return nil, status.Errorf(codes.FailedPrecondition, "no live run for session %q run %q", sessionID, runID)
	}
	actor, role, _ := PrincipalFromContext(ctx)
	targetType, targetID := "session", sessionID
	if runID != "" {
		targetType, targetID = "run", runID
	}
	deduped, err := s.recordAndEmit(ctx, domain.AuditEntry{
		ID: newAuditID(), CommandID: req.GetCommandId(), At: time.Now().UTC(),
		Actor: actor, Role: string(role), ActionType: "cancel_run",
		TargetType: targetType, TargetID: targetID,
		After: "cancelled", Reason: req.GetReason(), Result: "ok",
	})
	if err != nil {
		return nil, err
	}
	if !deduped {
		n := s.controls.CancelRun(sessionID, runID, &RunCancelledError{Actor: actor, Reason: req.GetReason()})
		slog.Warn("operator cancelled a run", "session", sessionID, "run", runID,
			"cancelled", n, "actor", actor, "reason", req.GetReason())
	}
	return &pb.CommandAck{CommandId: req.GetCommandId(), Deduped: deduped}, nil
}

// liveRuns counts the tracked Executes CancelRun(sessionID, runID) would cancel.
func (h *ExecutionControlHub) liveRuns(sessionID, runID string) int {
	if h == nil || (sessionID == "" && runID == "") {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for t := range h.runs {
		if sessionID != "" && t.session != domain.SessionID(sessionID) {
			continue
		}
		if runID != "" && !t.hasRun(domain.RunID(runID)) {
			continue
		}
		n++
	}
	return n
}

// PrincipalNames is the slice of the principal registry WhoAmI reads.
type PrincipalNames interface {
	GetPrincipal(ctx context.Context, id string) (domain.Principal, error)
}

// WhoAmI answers who the caller's bearer resolved to (contract 0102; ADR-0142 D1,
// T-20261106-operator-plane-has-no-whoami-an-oidc-signed-in). An OIDC-signed-in
// console never calls Login, so before this it showed the person from ID-token
// claims and guessed the plane role by probing Snapshot. Open to every
// authenticated role (memberMethods, isOperatorOnly).
//
// The bearer is resolved again rather than read from the interceptor's context
// value: the interceptor keeps only the derived plane role, and WhoAmI owes the
// caller the full role set and the issuer.
func (s *Service) WhoAmI(ctx context.Context, _ *pb.WhoAmIRequest) (*pb.WhoAmIResponse, error) {
	if s.idp == nil {
		return nil, status.Error(codes.Unimplemented, "operator identity not configured")
	}
	id, err := s.idp.Resolve(ctx, bearerToken(ctx))
	if err != nil || id.Principal.IsZero() {
		return nil, status.Error(codes.Unauthenticated, "invalid or expired token")
	}
	out := &pb.WhoAmIResponse{
		Principal:  id.Principal.String(),
		Role:       string(RoleFromRoles(id.Roles)),
		Roles:      append([]string(nil), id.Roles...),
		RoleSource: "kernel",
		Issuer:     id.Issuer,
	}
	if s.principalNames != nil {
		// Best effort: a registry miss leaves the name empty, never fails the call.
		if p, perr := s.principalNames.GetPrincipal(ctx, id.Principal.ID); perr == nil {
			out.DisplayName = p.DisplayName
		}
	}
	return out, nil
}
