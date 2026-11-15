package domain

import (
	"context"
	"strings"
	"time"
)

// ApprovalRequest is one decision a person must make: a dangerous tool call
// (ADR-0039 D10), an agent's request for a skill outside its effective set
// (Kind ApprovalKindSkillAccess, decisions/skill-access-grants.md), a proposed
// schedule, a proactive plan, a machine consent or choice, an account
// confirmation, a publish. Every kind rides the one addressed hub
// (AddressedHub, ADR-0142 D11), which routes it to its approvers by policy.
type ApprovalRequest struct {
	ID          string
	AgentID     string
	ToolName    string // for skill_access: "skill:<name>", so a console that predates Kind still renders it
	ArgsPreview string
	// Kind is "" for a dangerous-tool approval, ApprovalKindSkillAccess for a skill
	// access request, etc. (the closed set in approval_policy.go).
	Kind string
	// Timeout, when > 0, replaces the hub's default grace window for this
	// request (skill access waits execution.skills.access_request_timeout_seconds).
	Timeout time.Duration

	// ActionClass overrides the policy class ("" ⇒ the policy derives it from
	// Kind and, for tools, approvals.tool_classes).
	ActionClass string
	// Resource keys a standing approval ("" ⇒ the tool / machine+tool name).
	Resource string
	// Object names the exact object acted on, where known.
	Object string
	// Machine / Candidates carry a machine consent or choice (ADR-0127).
	Machine    string
	Candidates []string
	// Beneficiary is the person the action is for — the subject of a "self"
	// rule. Kernel-derived (conversation owner, task beneficiary, schedule
	// owner), never from a payload (INV-5).
	Beneficiary PrincipalRef
	// ConversationID routes delivery to the ordering conversation (the bridge).
	ConversationID string
	TaskID         string
	SessionID      string
	// Args is the call's full argument JSON, for the approval POLICY only (ADR-0142
	// stage 2: a limit such as "expense amount > 5000 escalates to the supervisor"
	// reads the measure from it). It is never persisted and never delivered to an
	// approver surface — ArgsPreview is the bounded, displayable form.
	Args []byte
	// DuplicateNotice tells the approver that an earlier side-effecting call of
	// this conversation matches this one ("Possible duplicate of … at …";
	// RenderDuplicateNotice), or "". Kernel-computed from the ActionHistory, never
	// from a payload; informational: it never changes routing or the decision
	// (T-20261107-approver-is-not-told-that-an-identical-side).
	DuplicateNotice string
	// Subtask is the label of the ADR-0150 subtask that raised the request, ""
	// for the main agent's own calls. Kernel-derived from the child lease; the
	// person sees it through DuplicateNotice (SubtaskApprovalNotice), so no
	// approval surface needs a new field.
	Subtask string
	// PersonalIdentifiers are the personal identifiers the outbound identifier
	// guard found in the call's text arguments (ENG-917), kernel-computed, never
	// from a payload. The approver sees them through DuplicateNotice
	// (RenderOutboundIdentifierNotice), so no approval surface needs a new field.
	// A request that carries them is never pre-approved by a standing approval
	// and cannot be answered with one: a person must see the identifiers.
	PersonalIdentifiers []OutboundIdentifier
}

// ApprovalDecision is the hub's answer to one request.
type ApprovalDecision struct {
	Approved   bool
	ApproverID string
	// Decided is true when someone actually ruled (approve OR deny), false when the
	// request failed closed without a ruling (no approver subscribed, timeout,
	// cancellation). Tool approvals treat both as a denial; skill access reports
	// the second as "pending" and keeps the request open for a later grant.
	Decided bool
	// State is the item's terminal state (approved, denied, expired, withdrawn,
	// needs_hierarchy, unroutable).
	State ApprovalState
	// Choice is a choose_machine answer.
	Choice string
	// ApprovalID is the hub item that carried the decision.
	ApprovalID string
	// Detail is why the item ended in State when nobody ruled (unroutable,
	// needs_hierarchy, …) or the resolver's note. Callers surface it verbatim so
	// a fail-closed request is never mistaken for a person saying no
	// (T-20261107-tool-approvals-raised-by-chat-delegated-plan).
	Detail string
}

// ApprovalController gates an action on a person's decision. Request blocks
// until decided or failed closed (no eligible approver / timeout / cancellation
// ⇒ a non-approved decision).
type ApprovalController interface {
	Request(ctx context.Context, req ApprovalRequest) (ApprovalDecision, error)
}

// ApprovalHub is the full approval surface the planes drive: requests, addressed
// delivery, answers checked at answer time, and standing approvals. Implemented
// by *AddressedHub — the only implementation (ADR-0142 D11).
type ApprovalHub interface {
	ApprovalController
	Raise(ctx context.Context, req ApprovalRequest) (ApprovalItem, error)
	Watch(spec ApprovalWatch) (<-chan ApprovalEvent, func())
	Answer(ctx context.Context, id string, ans ApprovalAnswer) (ApprovalItem, error)
	Answerable(ctx context.Context, it ApprovalItem, by Approver) (bool, string)
	Get(ctx context.Context, id string) (ApprovalItem, error)
	List(ctx context.Context, q ApprovalQuery) ([]ApprovalItem, error)
	ListStanding(ctx context.Context, q StandingQuery) ([]StandingApproval, error)
	RevokeStanding(ctx context.Context, id string, by Approver, reason string) (StandingApproval, error)
}

var _ ApprovalHub = (*AddressedHub)(nil)

// AlwaysApproveController is a non-interactive ApprovalController that approves
// every dangerous-tool call (ADR-0039). It is installed only when the operator
// opts in via ExecutionConfig.ToolsAutoApprove — a trusted/dev bypass of the
// human-in-the-loop gate, which is unanswerable in an unattended run. The
// per-call process confinement (jail + scrubbed env + caps + timeout) remains
// the containment boundary; this only removes the human decision.
type AlwaysApproveController struct{}

// Request always approves, stamped with a synthetic approver for the audit trail.
func (AlwaysApproveController) Request(_ context.Context, _ ApprovalRequest) (ApprovalDecision, error) {
	return ApprovalDecision{Approved: true, ApproverID: "auto-approve", Decided: true}, nil
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

// ApprovalDenyReason renders a non-approved decision as the deny reason an agent
// reads (T-20261107-tool-approvals-raised-by-chat-delegated-plan). It always
// starts "not approved", then says WHICH kind of no it was, because the right
// next move differs: a person who denied, or a window that closed, may be
// worth asking about again; a request that failed closed without a ruling
// (unroutable, needs_hierarchy, …) will be refused identically on every retry,
// so the reason says so in words the model cannot read as "try again".
func ApprovalDenyReason(dec ApprovalDecision, err error) string {
	const noRetry = " Retrying this call will be refused the same way; report that it needs approval and could not be routed."
	if err != nil {
		return "not approved: the approval could not be raised (" + err.Error() + ")"
	}
	detail := strings.TrimSpace(dec.Detail)
	with := func(s string) string {
		if detail == "" {
			return s
		}
		return s + ": " + detail
	}
	switch dec.State {
	case ApprovalDenied:
		if dec.ApproverID != "" {
			return with("not approved: denied by " + dec.ApproverID)
		}
		return with("not approved: denied")
	case ApprovalExpired:
		return "not approved: nobody answered the approval before it expired"
	case ApprovalWithdrawn:
		return with("not approved: the approval was withdrawn")
	case ApprovalUnroutable, ApprovalNeedsHierarchy:
		return with("not approved: "+string(dec.State)) + "." + noRetry
	case "":
		return "not approved"
	}
	return with("not approved: " + string(dec.State))
}
