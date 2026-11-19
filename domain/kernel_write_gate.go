package domain

import (
	"context"
	"log/slog"
)

// The Rule of Two on dedicated kernel write calls (ENG-2247, owner decision
// 2026-11-19: "gate every write call too"; T-20261119-rule-of-two-never-fires-on-the-default).
//
// The chat agent's writes are not tool calls: propose_memory_note, schedule_task,
// propose_skill, propose_knowledge, propose_ontology_change, forget and an
// agent's own ingest each reach a dedicated agent-plane RPC, so the gate inside
// ToolExecutor.Execute never saw them and the Rule of Two never fired on a
// default deployment (whose composition carries no write tools at all). These
// two methods put those calls in front of the SAME gate object (the executor's
// constructor-injected RuleOfTwo control — one rule, one tracker, no second
// implementation) and the SAME approval controller a gated tool call uses.
// Each verb's effect classes come from configuration
// (execution.tools.rule_of_two.kernel_write_effects), never a Go branch.

// KernelWriteCall is one dedicated kernel write call, as the gate and the
// approver see it. The taint scope travels on ctx (WithTaintScope), set by the
// transport from the lease binding exactly as for a tool call.
type KernelWriteCall struct {
	// Verb is the agent-facing verb (propose_memory_note, schedule_task, …): the
	// key of the configured effect map and the name the approver is shown.
	Verb string
	// AgentID is the calling agent (kernel-resolved, from the lease).
	AgentID string
	// ArgsPreview is what the approver reads about the call.
	ArgsPreview string
	// Args is the call's full arguments (the approval's argument record).
	Args []byte
}

// KernelWriteVerdict is the gate's answer for a kernel write call.
type KernelWriteVerdict struct {
	// Gated reports that the call completed the Rule of Two and went to approval.
	Gated bool
	// Allowed reports that the call may proceed: not gated, or approved.
	Allowed bool
	// ApproverID is who approved a gated call.
	ApproverID string
	// DenyReason is why a gated call may not proceed (the person declined, the
	// approval could not be routed, or no approval path is wired).
	DenyReason string
	// ApprovalState is the approval item's terminal state for a gated call.
	ApprovalState ApprovalState
}

// RuleOfTwoKernelWriteNotice reports whether a kernel write call completes the
// Rule of Two and, when it does, the "Rule of Two:" notice that names why. For
// a verb that raises its OWN approval (schedule_task): it adds the notice to
// that approval instead of asking the person twice. "" / false when the gate is
// off or the call does not complete all three legs.
func (e *ToolExecutor) RuleOfTwoKernelWriteNotice(ctx context.Context, call KernelWriteCall) (string, bool) {
	hit, gated := e.checkKernelWrite(ctx, call)
	if !gated {
		return "", false
	}
	return RenderRuleOfTwoNotice(hit), true
}

// GateKernelWrite decides a kernel write call that raises no approval of its
// own: a call that completes the Rule of Two is narrowed to ask, through the
// executor's approval controller, with the Rule-of-Two notice leading — the path
// a gated tool call takes. With no approval controller a gated call is denied
// (fail closed). A call the gate does not stop is allowed without asking.
func (e *ToolExecutor) GateKernelWrite(ctx context.Context, call KernelWriteCall) KernelWriteVerdict {
	hit, gated := e.checkKernelWrite(ctx, call)
	if !gated {
		return KernelWriteVerdict{Allowed: true}
	}
	slog.InfoContext(ctx, "ENG-2247: Rule of Two — tainted session with sensitive access makes a kernel write call; approval required",
		"verb", call.Verb, "agent", call.AgentID, "sources", hit.Sources,
		"unknown_provenance", hit.UnknownProvenance, "effects", hit.Effects)
	if e.approval == nil {
		return KernelWriteVerdict{Gated: true, DenyReason: RuleOfTwoDenyReason(hit)}
	}
	beneficiary := TaskBeneficiaryFromContext(ctx)
	if beneficiary.IsZero() {
		beneficiary = PersonPrincipal(ConversationOwnerFromContext(ctx))
	}
	sid, _ := SessionIDFromContext(ctx)
	dec, err := e.approval.Request(ctx, ApprovalRequest{
		AgentID: call.AgentID, ToolName: call.Verb, ArgsPreview: call.ArgsPreview,
		Beneficiary: beneficiary, Args: call.Args,
		ConversationID: ConversationIDFromContext(ctx), SessionID: string(sid),
		DuplicateNotice: RenderRuleOfTwoNotice(hit),
	})
	if err != nil || !dec.Approved {
		reason := ApprovalDenyReason(dec, err)
		slog.InfoContext(ctx, "ENG-2247: kernel write not approved (Rule of Two)", "verb", call.Verb,
			"agent", call.AgentID, "state", string(dec.State), "approval", dec.ApprovalID, "reason", reason)
		return KernelWriteVerdict{Gated: true, DenyReason: reason, ApprovalState: dec.State, ApproverID: dec.ApproverID}
	}
	return KernelWriteVerdict{Gated: true, Allowed: true, ApproverID: dec.ApproverID, ApprovalState: dec.State}
}

// checkKernelWrite runs the one gate on a kernel write call: the verb's
// configured effects as the call's effects, the ctx taint scope, and the calling
// agent's grants for the sensitive-access leg.
func (e *ToolExecutor) checkKernelWrite(ctx context.Context, call KernelWriteCall) (RuleOfTwoHit, bool) {
	if e == nil || e.ruleOfTwo == nil {
		return RuleOfTwoHit{}, false
	}
	return e.ruleOfTwo.Check(TaintScopeFromContext(ctx), e.ruleOfTwo.KernelWriteTool(call.Verb), func() ([]string, error) {
		return e.grantedTags(ctx, call.AgentID)
	})
}
