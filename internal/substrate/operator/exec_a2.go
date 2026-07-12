package operator

import (
	"context"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"
	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// ToolRunner is the kernel tool reference monitor the operator plane drives for
// ScopeSystem execution (ADR-0047 A2.2). Satisfied by *domain.ToolExecutor.
type ToolRunner interface {
	Execute(ctx context.Context, req domain.ToolCallRequest) domain.ToolCallResponse
}

// SetToolExec wires the tool runner for operator ExecuteTool. nil ⇒ Unimplemented.
func (s *Service) SetToolExec(runner ToolRunner) { s.toolRunner = runner }

// ExecuteTool runs a tool at ScopeSystem on the operator's authority (A2.2).
// Idempotent on command_id: a retry returns the original result from the audit
// row without re-running the tool (tool execution is not naturally idempotent).
// The audit write (which the feed also folds as an AuditEvent) is the visibility
// record — a privileged operator execution is never silent (D13/D15).
func (s *Service) ExecuteTool(ctx context.Context, req *pb.ExecuteToolOpRequest) (*pb.ExecuteToolOpResponse, error) {
	if req.GetCommandId() == "" || req.GetReason() == "" {
		return nil, status.Error(codes.InvalidArgument, "command_id and reason are required")
	}
	if req.GetToolName() == "" {
		return nil, status.Error(codes.InvalidArgument, "tool_name is required")
	}
	if s.audit == nil {
		return nil, status.Error(codes.Unimplemented, "operator audit store not configured")
	}
	if s.toolRunner == nil {
		return nil, status.Error(codes.Unimplemented, "operator tool executor not configured")
	}

	// Idempotency: return the original result for a replayed command_id without
	// re-running. The audit `after` holds the result_json. Query-first (a human
	// operator retry is sequential); a rare concurrent duplicate is caught by the
	// audit UNIQUE on Record below, at worst running the tool twice.
	prior, err := s.audit.Query(ctx, AuditFilter{CommandID: req.GetCommandId(), Limit: 1})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "audit lookup: %v", err)
	}
	if len(prior) == 1 {
		return &pb.ExecuteToolOpResponse{CommandId: req.GetCommandId(), Deduped: true, ResultJson: prior[0].After}, nil
	}

	resp := s.toolRunner.Execute(ctx, domain.ToolCallRequest{
		System:   true, // ScopeSystem: bypass per-agent grant/scope/approval (A2.2)
		ToolName: req.GetToolName(),
		ArgsJSON: []byte(req.GetArgsJson()),
		// req.session_id is informational (audit/UI context); ToolCallRequest has no
		// plain session field — ScopeSystem execution is not session-scoped.
	})

	actor, role, _ := PrincipalFromContext(ctx)
	entry := domain.AuditEntry{
		ID: newAuditID(), CommandID: req.GetCommandId(), At: time.Now().UTC(),
		Actor: actor, Role: string(role), ActionType: "execute_tool",
		TargetType: "tool", TargetID: req.GetToolName(),
		Before: toolDangerLabel(s.toolDangerous(req.GetToolName())), // "dangerous" | ""
		After:  string(resp.ResultJSON),
		Reason: req.GetReason(), Result: execResultLabel(resp),
	}
	if _, err := s.recordAndEmit(ctx, entry); err != nil {
		return nil, err
	}
	return &pb.ExecuteToolOpResponse{
		CommandId:  req.GetCommandId(),
		Deduped:    false,
		ResultJson: string(resp.ResultJSON),
		Denied:     resp.Denied,
		DenyReason: resp.DenyReason,
		Error:      resp.Error,
	}, nil
}

// toolDangerous reports whether the named tool is dangerous, from the catalog.
// Unknown/no-catalog ⇒ false (the label is advisory audit metadata).
func (s *Service) toolDangerous(name string) bool {
	if s.tools == nil {
		return false
	}
	for _, t := range s.tools.AllTools() {
		if t.Name == name {
			return t.Dangerous
		}
	}
	return false
}

func toolDangerLabel(dangerous bool) string {
	if dangerous {
		return "dangerous"
	}
	return ""
}

func execResultLabel(resp domain.ToolCallResponse) string {
	switch {
	case resp.Denied:
		return "denied: " + resp.DenyReason
	case resp.Error != "":
		return "error: " + resp.Error
	default:
		return "ok"
	}
}

// SetToolPolicy binds an existing grant with a resource policy (A2.3). Setting a
// policy on an agent that lacks the grant is InvalidArgument — grant it first via
// SetToolGrant. Idempotent + audited via runMutation.
func (s *Service) SetToolPolicy(ctx context.Context, req *pb.SetToolPolicyRequest) (*pb.CommandAck, error) {
	if s.grants == nil {
		return nil, status.Error(codes.Unimplemented, "operator grants store not configured")
	}
	if req.GetAgentId() == "" || req.GetToolName() == "" {
		return nil, status.Error(codes.InvalidArgument, "agent_id and tool_name are required")
	}
	policy := fromToolPolicyOp(req.GetPolicy())
	after := req.GetToolName() + ":" + policySummary(policy)
	return s.runMutation(ctx, req.GetCommandId(), req.GetReason(), "set_tool_policy", "agent", req.GetAgentId(),
		after, func() error {
			current, err := s.grants.GrantsFor(ctx, req.GetAgentId())
			if err != nil {
				return status.Errorf(codes.Internal, "read grants: %v", err)
			}
			updated := make([]domain.ToolGrant, len(current))
			found := false
			for i, g := range current {
				if g.Tool == req.GetToolName() {
					g.Policy = policy
					found = true
				}
				updated[i] = g
			}
			if !found {
				return status.Errorf(codes.InvalidArgument, "agent %q has no grant for tool %q; grant it first", req.GetAgentId(), req.GetToolName())
			}
			s.grants.Set(req.GetAgentId(), updated)
			return nil
		})
}

func fromToolPolicyOp(p *pb.ToolPolicyOp) domain.ToolResourcePolicy {
	if p == nil {
		return domain.ToolResourcePolicy{}
	}
	return domain.ToolResourcePolicy{
		Filesystem: domain.FilesystemPolicy{AllowRoots: p.GetAllowedPaths()},
		Network:    domain.NetworkPolicy{AllowDomains: p.GetAllowedUrls()},
		Command:    domain.CommandPolicy{AllowCommands: p.GetAllowedCommands()},
	}
}

// policySummary renders a stable audit `after` value for a resource policy.
func policySummary(p domain.ToolResourcePolicy) string {
	parts := []string{
		"paths=" + strings.Join(p.Filesystem.AllowRoots, "|"),
		"urls=" + strings.Join(p.Network.AllowDomains, "|"),
		"cmds=" + strings.Join(p.Command.AllowCommands, "|"),
	}
	return strings.Join(parts, ";")
}
