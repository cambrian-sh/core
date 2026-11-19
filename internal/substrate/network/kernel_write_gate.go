package network

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
)

// The Rule of Two on the dedicated kernel write RPCs (ENG-2247, owner decision
// 2026-11-19: gate every write call too;
// T-20261119-rule-of-two-never-fires-on-the-default).
//
// ProposeMemoryNote, ProposeSkill, ProposeKnowledge, ProposeOntologyChange,
// ManageScheduledTask, Forget and an agent's own IngestMemory ask the tool
// executor's Rule-of-Two gate (the one ENG-531 control) before they write; a
// call that completes the rule goes to approval through the executor's approval
// controller. ProposeScheduledTask raises its own approval, so it only adds the
// Rule-of-Two notice to it (ruleOfTwoNotice) — the person is never asked twice.
//
// The taint scope is the one a tool call and a recall use (withTaintScope, from
// the lease binding), so what a turn recalled taints the write the same turn asks
// for. A call that presents no lease is not a session's write — the lease-bound
// verbs refuse such a call before they get here, and a lease-less IngestMemory or
// Forget is a connector's own ingestion (ENG-494 source principals) or an
// unattached caller, which the gate cannot attribute to any session's reads.

// kernelWriteContext resolves the presented lease (else the x-lease-id header)
// and stamps what the approval needs from the binding — never from the request:
// the conversation owner (who is asked), the task session and its beneficiary,
// the ordering conversation — then the Rule-of-Two taint scope. ok=false when no
// lease was presented.
func (s *Server) kernelWriteContext(ctx context.Context, leaseID string) (context.Context, domain.LeaseID, domain.LeaseBinding, bool) {
	lease := domain.LeaseID(leaseID)
	if lease == "" {
		lease = domain.LeaseID(mdValue(ctx, leaseHeader))
	}
	if lease == "" {
		return ctx, "", domain.LeaseBinding{}, false
	}
	b, known := s.resolveLeaseFor(ctx, lease)
	if s.ConversationOwner != nil && domain.ConversationOwnerFromContext(ctx) == "" {
		ctx = s.stampConversationOwner(ctx, b, known)
	}
	if known {
		if _, has := domain.SessionIDFromContext(ctx); !has && b.SessionID != "" {
			ctx = domain.WithSessionID(ctx, b.SessionID)
			if s.TaskOwners != nil {
				if owner := s.TaskOwners.BeneficiaryOf(b.SessionID); !owner.IsZero() {
					ctx = domain.WithTaskBeneficiary(ctx, owner)
				}
			}
		}
		if domain.ConversationIDFromContext(ctx) == "" {
			if conv := s.conversationOfBinding(ctx, b); conv != "" {
				ctx = domain.WithConversationID(ctx, conv)
			}
		}
	}
	return s.withTaintScope(ctx, lease), lease, b, true
}

// kernelWriteCall builds the gate's view of one call. The agent is the lease's,
// else the authenticated caller's.
func kernelWriteCall(ctx context.Context, b domain.LeaseBinding, verb, preview string, args map[string]any) domain.KernelWriteCall {
	agentID := b.AgentID
	if agentID == "" {
		agentID, _ = callerAgentIdentity(ctx)
	}
	raw, _ := json.Marshal(args) // best effort: the approval's argument record; a map of strings always marshals
	return domain.KernelWriteCall{Verb: verb, AgentID: agentID, ArgsPreview: preview, Args: raw}
}

// gateKernelWrite asks the Rule-of-Two gate about one kernel write call. It
// returns ok=true when the write may go ahead (no tool executor or gate wired, no
// lease presented, the rule not completed, or the person approved), otherwise
// the refusal the RPC reports. A gated call waits on the person like a gated tool
// call: admitted as the turn's work (admitTurnWork), so the wait is not silence
// and the turn's end withdraws the approval.
func (s *Server) gateKernelWrite(ctx context.Context, leaseID, verb, preview string, args map[string]any) (string, bool) {
	if s.ToolExecutor == nil || s.ToolExecutor.RuleOfTwo() == nil {
		return "", true
	}
	ctx, lease, b, ok := s.kernelWriteContext(ctx, leaseID)
	if !ok {
		return "", true
	}
	call := kernelWriteCall(ctx, b, verb, preview, args)
	if _, gated := s.ToolExecutor.RuleOfTwoKernelWriteNotice(ctx, call); !gated {
		return "", true
	}
	ctx, release, admitted := s.admitTurnWork(ctx, lease)
	if !admitted {
		return ErrTurnEnded.Error() + "; nothing was written", false
	}
	defer release()
	v := s.ToolExecutor.GateKernelWrite(ctx, call)
	if v.Allowed {
		slog.InfoContext(ctx, "ENG-2247: kernel write approved after the Rule of Two", "verb", verb,
			"agent", call.AgentID, "approver", v.ApproverID)
		return "", true
	}
	return v.DenyReason, false
}

// ruleOfTwoNotice is the Rule-of-Two notice for a kernel write call that raises
// its own approval (ProposeScheduledTask): "" when the gate is off, no lease was
// presented or the call does not complete the rule.
func (s *Server) ruleOfTwoNotice(ctx context.Context, leaseID, verb, preview string, args map[string]any) string {
	if s.ToolExecutor == nil || s.ToolExecutor.RuleOfTwo() == nil {
		return ""
	}
	ctx, _, b, ok := s.kernelWriteContext(ctx, leaseID)
	if !ok {
		return ""
	}
	call := kernelWriteCall(ctx, b, verb, preview, args)
	notice, gated := s.ToolExecutor.RuleOfTwoKernelWriteNotice(ctx, call)
	if gated {
		slog.InfoContext(ctx, "ENG-2247: Rule of Two — the call's own approval carries the notice", "verb", verb, "agent", call.AgentID)
	}
	return notice
}

// previewText is at most n runes of s on one line, for an approver's preview.
func previewText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// forgetPreview names what an agent-plane erasure would erase, for the approver.
func forgetPreview(req *pb.ForgetRequest) string {
	sel := req.GetSelector()
	parts := []string{"forget"}
	add := func(kind string, ids []string) {
		if len(ids) > 0 {
			parts = append(parts, kind+" "+previewText(strings.Join(ids, ", "), 120))
		}
	}
	add("documents", sel.GetDocumentIds())
	add("threads", sel.GetThreadIds())
	add("chunks", sel.GetChunkIds())
	add("experiences", sel.GetExperienceIds())
	add("entities", sel.GetEntityRefs())
	add("memory notes", sel.GetMemoryNoteIds())
	if req.GetPreviewToken() != "" {
		parts = append(parts, "previewed erasure "+req.GetPreviewToken())
	}
	return strings.Join(parts, "; ")
}
