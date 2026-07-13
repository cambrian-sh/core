package operator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// GrantsStore is the subset of the kernel grants store the operator plane needs
// to read+write tool grants. Satisfied by domain.InMemoryGrantsStore.
type GrantsStore interface {
	GrantsFor(ctx context.Context, agentID string) ([]domain.ToolGrant, error)
	Set(agentID string, grants []domain.ToolGrant)
}

// SetCommandSources wires the audit store and grants store used by mutating
// commands (ADR-0047 D15). Without them the command RPCs return Unimplemented.
func (s *Service) SetCommandSources(audit AuditStore, grants GrantsStore) {
	s.audit = audit
	s.grants = grants
}

// recordAndEmit is the audit decorator shared by every mutating command. It
// persists the entry (the UNIQUE command_id is the dedup) and, only when the
// write is new, emits an AuditEvent on the feed — write-then-emit, so a client
// folding the event always finds the durable row. ADR-0047 D15.
func (s *Service) recordAndEmit(ctx context.Context, entry domain.AuditEntry) (deduped bool, err error) {
	if s.audit == nil {
		return false, status.Error(codes.Unimplemented, "operator audit store not configured")
	}
	deduped, err = s.audit.Record(ctx, entry)
	if err != nil {
		return false, err
	}
	if !deduped {
		s.feed.Emit(domain.AuditEvent{Entry: entry})
	}
	return deduped, nil
}

// SetToolGrant grants or revokes a system tool for an agent (ADR-0047 D7/D15).
// Operator-only (enforced by the interceptor). Idempotent on command_id;
// mandatory reason; before/after captured from the grants store's view.
func (s *Service) SetToolGrant(ctx context.Context, req *pb.SetToolGrantRequest) (*pb.CommandAck, error) {
	if s.grants == nil {
		return nil, status.Error(codes.Unimplemented, "operator grants store not configured")
	}
	if req.GetReason() == "" {
		return nil, status.Error(codes.InvalidArgument, "reason is required for a mutating command")
	}
	if req.GetCommandId() == "" {
		return nil, status.Error(codes.InvalidArgument, "command_id is required")
	}
	if req.GetAgentId() == "" || req.GetToolName() == "" {
		return nil, status.Error(codes.InvalidArgument, "agent_id and tool_name are required")
	}

	actor, role, _ := PrincipalFromContext(ctx) // interceptor guarantees an operator principal

	before, err := s.grants.GrantsFor(ctx, req.GetAgentId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read grants: %v", err)
	}
	after := applyGrant(before, req.GetToolName(), req.GetGranted())

	entry := domain.AuditEntry{
		ID:         newAuditID(),
		CommandID:  req.GetCommandId(),
		At:         time.Now().UTC(),
		Actor:      actor,
		Role:       string(role),
		ActionType: "set_tool_grant",
		TargetType: "agent",
		TargetID:   req.GetAgentId(),
		Before:     toolNamesJSON(before),
		After:      toolNamesJSON(after),
		Reason:     req.GetReason(),
		Result:     "ok",
	}

	deduped, err := s.recordAndEmit(ctx, entry)
	if err != nil {
		return nil, err
	}
	// Apply the mutation only when the command is new — a retried command_id is a
	// no-op (the original effect already happened).
	if !deduped {
		s.grants.Set(req.GetAgentId(), after)
	}
	return &pb.CommandAck{CommandId: req.GetCommandId(), Deduped: deduped}, nil
}

// applyGrant adds or removes a tool from a grant set, preserving existing policy.
func applyGrant(current []domain.ToolGrant, tool string, granted bool) []domain.ToolGrant {
	out := make([]domain.ToolGrant, 0, len(current)+1)
	found := false
	for _, g := range current {
		if g.Tool == tool {
			found = true
			if granted {
				out = append(out, g) // keep
			}
			continue // when revoking, drop it
		}
		out = append(out, g)
	}
	if granted && !found {
		out = append(out, domain.ToolGrant{Tool: tool})
	}
	return out
}

func newAuditID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
