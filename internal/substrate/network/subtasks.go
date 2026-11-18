package network

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/cambrian-sh/core/domain"
)

// ADR-0150 D2: the agent-plane half of run_subtasks (internal/subtasks). The
// kernel tool decides what runs; the server runs each subtask on a FRESH worker:
//
//   - a child lease minted by the gateway and bound like the parent's (session,
//     run, step, conversation, act-for, delegation depth), so ADR-0131 session
//     spend, ADR-0142 act-for and ADR-0146 own-rows apply as for the parent, and
//     every model and tool call of the worker is recorded under the parent's run;
//   - registered in Subtasks with its binding (tools, effect ceiling, depth, label):
//     the ToolExecutor enforces the tool and effect ceilings and keys the worker's
//     side-effecting calls deterministically; Execute refuses its delegation;
//   - the composed runtime (execution.workers.composed), with a composition of
//     exactly the subtask's tools, the brief's skills, the rendered brief and the
//     input ledger refs — no parent transcript;
//   - a dedicated runtime instance, so subtasks run in parallel and never queue
//     behind the parent's own process.

var _ domain.SubtaskRunner = (*Server)(nil)

// RegisterSubtaskParent marks lease as the single-agent lane agent's (depth 0):
// run_subtasks is offered and allowed on it. Called by the lane when it binds the
// lease (executeSingleAgent; the merged chat turn). A no-op without subtasks.
func (s *Server) RegisterSubtaskParent(lease domain.LeaseID, agentID string) {
	s.Subtasks.RegisterParent(lease, agentID)
}

// SubtaskParentTools is the parent's tool menu (what ListTools serves its lease,
// kernel tools excluded): the subtask worker's tools are a subset of it.
func (s *Server) SubtaskParentTools(ctx context.Context, agentID string, lease domain.LeaseID) []domain.SystemTool {
	if s.ToolExecutor == nil {
		return nil
	}
	tools := s.ToolExecutor.AvailableTools(ctx, agentID)
	tools = s.withComposedTools(ctx, agentID, string(lease), tools)
	kept := tools[:0:0]
	for _, t := range tools {
		if s.ToolExecutor.WithinLeaseCeiling(string(lease), t.Name) {
			kept = append(kept, t)
		}
	}
	return kept
}

// RunSubtask implements domain.SubtaskRunner.
func (s *Server) RunSubtask(ctx context.Context, d domain.SubtaskDispatch) (out domain.SubtaskOutcome, err error) {
	if s.Subtasks == nil || s.LLMGateway == nil || s.Dispatcher == nil {
		return domain.SubtaskOutcome{}, domain.ErrSubtaskUnavailable
	}
	if s.Compositions == nil || s.ComposedRuntimeID == "" {
		return domain.SubtaskOutcome{}, fmt.Errorf("%w: subtask workers run on the composed runtime (execution.workers.composed is off)", domain.ErrSubtaskUnavailable)
	}
	runtime := s.ComposedRuntimeID
	parent, known := domain.LeaseBinding{}, false
	if r := s.leaseResolver(); r != nil {
		parent, known = r.ResolveLease(d.Binding.ParentLease)
	}
	if !known {
		return domain.SubtaskOutcome{}, fmt.Errorf("%w: the parent's lease is not known", domain.ErrSubtaskUnavailable)
	}
	// A parent running on the third-party runtime fans out on it too (ENG-2083):
	// its children hold a slice of a composition with a third-party profession,
	// so they never move to the shared system-tier runtime.
	if s.ComposedThirdPartyRuntimeID != "" {
		if parent.AgentID == s.ComposedThirdPartyRuntimeID {
			runtime = s.ComposedThirdPartyRuntimeID
		} else if pc, ok := s.Compositions.For(string(d.Binding.ParentLease)); ok && pc.Runtime == s.ComposedThirdPartyRuntimeID {
			runtime = s.ComposedThirdPartyRuntimeID
		}
	}

	child, err := s.LLMGateway.Acquire(ctx, domain.StepAllocation{}, StepLeaseTokenLimit, StepLeaseDuration)
	if err != nil {
		return domain.SubtaskOutcome{}, fmt.Errorf("subtask lease: %w", err)
	}
	step := parent.StepIndex
	if step < 0 && parent.RunID != "" {
		step = 0
	}
	if r := s.leaseResolver(); r != nil {
		r.BindLease(child, domain.LeaseBinding{
			SessionID: parent.SessionID, RunID: parent.RunID, StepIndex: step, AgentID: runtime,
			ConversationID: parent.ConversationID, OriginMessageID: parent.OriginMessageID,
			ActedBy: parent.ActedBy, DelegationDepth: parent.DelegationDepth, PlanGeneration: parent.PlanGeneration,
		})
	}
	s.Subtasks.Bind(child, d.Binding)
	comp := domain.WorkerComposition{
		Runtime: runtime, Skills: d.Skills, Tools: d.Binding.Tools, Brief: d.Task,
		LedgerRefs: d.Inputs, SystemPrompt: "composed_worker", RunID: string(parent.RunID), StepIndex: step,
	}.Normalize()
	s.Compositions.Bind(child, comp)
	if s.ToolExecutor != nil && s.ToolExecutor.Overlay != nil && len(comp.Tools) > 0 {
		s.ToolExecutor.Overlay.Activate(string(child), comp.Tools)
	}
	// out is the named result, so what the deferred read observes reaches the caller.
	defer func() {
		// The kernel's own record of what the worker did, read before the lease
		// (and its registry entry) ends.
		out.Evidence, out.ReadWeb = s.Subtasks.Report(child)
		s.Subtasks.Forget(child)
		_, _ = s.LLMGateway.Complete(context.WithoutCancel(ctx), child)
	}()

	handoff := &domain.Handoff{
		Payload: &domain.Payload{Type: "task", Data: []byte(d.Task)},
		Context: map[string]string{
			"task_id":                    "subtask-" + d.Binding.SubtaskID,
			"_step_index":                strconv.Itoa(step),
			"original_prompt":            d.Task,
			"_session_token_id":          string(child),
			domain.HandoffKeyComposition: comp.Encode(),
			domain.HandoffKeySubtask:     d.Binding.SubtaskID,
		},
	}
	// ADR-0150 D4 on the worker (ENG-1760, owner 2026-11-18 "force a summary"):
	// the lane's stopping rule runs in the worker loop (sdk stopping.py) — at the
	// tool-round or wall-time cap, or stuck, it makes ONE tool-less summary call
	// and answers partial with its exit_reason, which the tool's classify reads.
	// The lane marker brings no ask-back here: the SDK never offers ask_person to
	// a request with a "_subtask" key (stopping.is_subtask, SDK >= 0.1.6).
	lim := s.laneLimits()
	lim.MaxToolRounds, lim.MaxWallSeconds = d.MaxToolRounds, d.MaxWallSeconds
	lim.Stamp(handoff.Context)
	actingForID, actingForName := s.actingForRun(ctx, parent.SessionID, parent.RunID)
	domain.StampActingFor(handoff.Context, actingForID, actingForName)

	// The parent's turn ended ⇒ its subtasks end (ctx is the parent's tool call).
	callCtx := domain.WithSubtaskOrigin(domain.WithDedicatedInstance(ctx), d.Binding)
	slog.InfoContext(ctx, "ADR-0150: subtask dispatched", "subtask", d.Binding.SubtaskID, "label", d.Binding.Label,
		"run", parent.RunID, "depth", d.Binding.Depth, "tools", len(comp.Tools), "runtime", runtime)
	resp, err := s.Dispatcher.CallAgent(callCtx, runtime, handoff, "")
	if err != nil {
		return out, err
	}
	if resp != nil && resp.Payload != nil {
		out.Text = string(resp.Payload.Data)
		out.PayloadType = resp.Payload.Type
	}
	out.Meta = map[string]string{}
	if resp != nil {
		for k, v := range resp.Context {
			out.Meta[k] = v
		}
		if resp.Payload != nil {
			for k, v := range resp.Payload.Metadata {
				out.Meta[k] = v
			}
		}
	}
	return out, nil
}
