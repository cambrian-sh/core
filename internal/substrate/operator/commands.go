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
// to read+write tool grants. Satisfied by *domain.LayeredGrantsStore (the kernel's,
// durable in Postgres since ENG-1116) and *domain.InMemoryGrantsStore. A write can
// fail (the durable store); a failed write is never applied nor audited as ok.
type GrantsStore interface {
	GrantsFor(ctx context.Context, agentID string) ([]domain.ToolGrant, error)
	ReplaceGrants(ctx context.Context, agentID string, grants []domain.ToolGrant) error
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

	if s.audit == nil {
		return nil, status.Error(codes.Unimplemented, "operator audit store not configured")
	}

	// ENG-1116: the grants store is durable, so the write can fail. Dedupe first
	// (a replayed command neither re-reads nor re-applies), apply, then record —
	// the runMutationBefore order — so a failed write leaves no "ok" audit row
	// that would make a retry of the same command_id a silent no-op.
	s.grantsMu.Lock()
	defer s.grantsMu.Unlock()
	prior, err := s.audit.Query(ctx, AuditFilter{CommandID: req.GetCommandId(), Limit: 1})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "audit lookup: %v", err)
	}
	if len(prior) == 1 {
		return &pb.CommandAck{CommandId: req.GetCommandId(), Deduped: true}, nil
	}

	actor, role, _ := PrincipalFromContext(ctx) // interceptor guarantees an operator principal

	before, err := s.grants.GrantsFor(ctx, req.GetAgentId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read grants: %v", err)
	}
	after := applyGrant(before, req.GetToolName(), req.GetGranted())
	if err := s.grants.ReplaceGrants(ctx, req.GetAgentId(), after); err != nil {
		return nil, status.Errorf(codes.Internal, "write grants: %v", err)
	}

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
	// A concurrent duplicate of this command_id (another kernel) may still dedupe
	// here; the grant write is idempotent for the same tool and direction.
	deduped, err := s.recordAndEmit(ctx, entry)
	if err != nil {
		return nil, err
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
