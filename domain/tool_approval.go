package domain

import "context"

// ApprovalRequest is a pending operator decision for a dangerous tool call
// (ADR-0039 D10).
type ApprovalRequest struct {
	ID          string
	AgentID     string
	ToolName    string
	ArgsPreview string
}

// ApprovalDecision is an operator's (or automated approver's) ruling.
type ApprovalDecision struct {
	Approved   bool
	ApproverID string
}

// ApprovalController gates dangerous tool calls on an operator decision
// (ADR-0039 D10). Request blocks until decided or times out; it is fail-closed
// (no approver / timeout ⇒ a non-approved decision). The operator-plane
// WatchApprovals / SubmitApprovalDecision RPCs drive the default implementation.
type ApprovalController interface {
	Request(ctx context.Context, req ApprovalRequest) (ApprovalDecision, error)
}

// ApprovalHub is the full approval surface the operator-plane RPCs drive:
// the ApprovalController (used by ToolExecutor) plus Watch (WatchApprovals
// stream) and Submit (SubmitApprovalDecision). InMemoryApprovalController
// satisfies it.
type ApprovalHub interface {
	ApprovalController
	Watch() (<-chan ApprovalRequest, func())
	Submit(id string, approved bool, approverID string) bool
}

// AlwaysApproveController is a non-interactive ApprovalController that approves
// every dangerous-tool call (ADR-0039). It is installed only when the operator
// opts in via ExecutionConfig.ToolsAutoApprove — a trusted/dev bypass of the
// human-in-the-loop gate, which is unanswerable in an unattended run. The
// per-call process confinement (jail + scrubbed env + caps + timeout) remains
// the containment boundary; this only removes the human decision.
type AlwaysApproveController struct{}

// Request always approves, stamped with a synthetic approver for the audit trail.
func (AlwaysApproveController) Request(_ context.Context, _ ApprovalRequest) (ApprovalDecision, error) {
	return ApprovalDecision{Approved: true, ApproverID: "auto-approve"}, nil
}

// EgressAuditor records that data left the trust boundary via a remote tool call
// (ADR-0043 D4). The call is allowed (the operator owns endpoint trust); this is
// the forensic trail of what was sent where, since enforcement is soft when the
// tool's data classes are undeclared. nil ⇒ no egress auditing.
type EgressAuditor interface {
	RecordEgress(agentID, toolName string, dataClasses []string)
}

// (The former ToolScopeResolver seam is gone: the data-store regime now asks
// domain.Authorizer for the principal's predicate, so tools, memory, skills, and
// artifacts all resolve access through one port. ADR-0085.)
