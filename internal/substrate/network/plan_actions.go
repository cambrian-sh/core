package network

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
)

// PlanActionsKey is the Execute response metadata key carrying the plan's ledger of
// side-effecting tool calls as JSON (planActionsJSON); absent when there were none.
// Kernel-authored on the way out; the same ledger is rendered into the payload text.
const PlanActionsKey = "_plan_actions"

// planActionLedger is the per-run record of the side-effecting tool calls a plan's
// steps made (domain.PlanAction), so Execute can tell its caller what the plan DID
// (T-20261107-plan-result-is-only-the-last-topo-step-a-plan). A run is recorded only
// between begin and end, so a tool call outside a running plan (a chat turn's own
// calls, a late call from a step whose plan already returned) records nothing and the
// map cannot grow without bound. The zero value is ready to use.
type planActionLedger struct {
	mu   sync.Mutex
	runs map[domain.RunID][]domain.PlanAction
	// denials are the run's refused READ calls (not actions: nothing changed), kept
	// for coverage's not_permitted check (ADR-0145 D5.3). Same lifetime as runs.
	denials map[domain.RunID][]domain.PlanAction
}

// begin opens run's record. A resumed run is begun again by its new Execute: the
// record restarts, because the result of that Execute reports what IT did.
func (l *planActionLedger) begin(run domain.RunID) {
	if run == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.runs == nil {
		l.runs = map[domain.RunID][]domain.PlanAction{}
	}
	l.runs[run] = nil
	delete(l.denials, run)
}

// record appends a to run's record; a run not begun records nothing.
func (l *planActionLedger) record(run domain.RunID, a domain.PlanAction) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, open := l.runs[run]; !open {
		return
	}
	l.runs[run] = append(l.runs[run], a)
}

// recordDenial appends a refused read to run's denials; a run not begun records
// nothing.
func (l *planActionLedger) recordDenial(run domain.RunID, a domain.PlanAction) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, open := l.runs[run]; !open {
		return
	}
	if l.denials == nil {
		l.denials = map[domain.RunID][]domain.PlanAction{}
	}
	l.denials[run] = append(l.denials[run], a)
}

// deniedReads returns a copy of run's refused reads.
func (l *planActionLedger) deniedReads(run domain.RunID) []domain.PlanAction {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]domain.PlanAction(nil), l.denials[run]...)
}

// snapshot returns a copy of run's record.
func (l *planActionLedger) snapshot(run domain.RunID) []domain.PlanAction {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]domain.PlanAction(nil), l.runs[run]...)
}

// unroutableRefusals counts run's calls of tool refused because their approval
// could not be routed to anyone (unroutable, needs_hierarchy): resolutions that a
// retry of the same tool in the same run meets again, whatever its arguments.
func (l *planActionLedger) unroutableRefusals(run domain.RunID, tool string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, a := range l.runs[run] {
		if a.Tool == tool && a.Status == domain.PlanActionRefused &&
			(a.ApprovalState == domain.ApprovalUnroutable || a.ApprovalState == domain.ApprovalNeedsHierarchy) {
			n++
		}
	}
	return n
}

// unroutableBound is ExecuteTool's per-run bound on repeated unroutable approvals
// (execution.tools.max_unroutable_approvals_per_run; part 2 of
// T-20261107-chat-agent-re-delegates-after-a-deterministic). Once a running plan
// has had max calls of a tool refused as unroutable, a further call of that tool in
// the run is refused HERE, without raising another approval: in run
// 20261107T114033Z_usecases_full one plan's steps raised 39 unroutable approvals for
// mail_draft/mail_send, varying the arguments each time, each one a persisted
// approval record and a fail-closed log line, none able to succeed. The refusal
// reuses the kernel's final-refusal wording (domain.ApprovalDenyReason), which the
// agents treat as final. ok=false means refuse with reason. Calls outside a running
// plan (no run-bound lease) are not bounded here: the run's ledger is the record.
func (s *Server) unroutableBound(ctx context.Context, lease domain.LeaseID, tool string) (reason string, ok bool) {
	max := s.ExecCfg.Tools.MaxUnroutableApprovalsPerRun
	if max <= 0 || lease == "" {
		return "", true
	}
	b, known := s.resolveLeaseFor(ctx, lease)
	if !known || b.RunID == "" {
		return "", true
	}
	n := s.planActions.unroutableRefusals(b.RunID, tool)
	if n < max {
		return "", true
	}
	slog.Warn("approvals: tool refused without asking: its approvals in this run were unroutable",
		"event", "approval_unroutable_bound", "run", b.RunID, "tool", tool, "refused", n, "max", max)
	return fmt.Sprintf("not approved: unroutable: %d calls of %s in this run could not be routed to an approver, so no further call of it is attempted in this run (execution.tools.max_unroutable_approvals_per_run).", n, tool) +
		" Retrying this call will be refused the same way; report that it needs approval and could not be routed.", false
}

// end closes run's record.
func (l *planActionLedger) end(run domain.RunID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.runs, run)
	delete(l.denials, run)
}

// recordPlanAction records one ExecuteTool outcome against the plan run its lease
// belongs to, when the tool is side-effecting (sideEffectingTool). The run and step
// come from the kernel's lease binding, never from the request. Best-effort and
// cheap: a call with no run-bound lease, or a read-only tool, records nothing.
func (s *Server) recordPlanAction(ctx context.Context, lease domain.LeaseID, req domain.ToolCallRequest, resp domain.ToolCallResponse) {
	if lease == "" {
		return
	}
	b, known := s.resolveLeaseFor(ctx, lease)
	if !known || b.RunID == "" {
		return
	}
	toolKnown, effecting := s.sideEffectingTool(req.ToolName)
	if !effecting {
		// ADR-0145 D5.3: a refused READ is the kernel's record that access was
		// refused, which is what coverage needs to accept not_permitted for an item
		// (T-20261108-coverage-cannot-accept-not-permitted-for-a). It is not an
		// action (nothing changed), so it is kept apart from the action ledger.
		if resp.Denied && toolKnown {
			s.planActions.recordDenial(b.RunID, domain.PlanAction{Step: b.StepIndex, Generation: b.PlanGeneration,
				Agent: req.AgentID, Tool: req.ToolName, Args: redactedArgs(string(req.ArgsJSON)),
				Status: domain.PlanActionRefused, Detail: resp.DenyReason, ApprovalState: resp.ApprovalState})
		}
		return
	}
	a := domain.PlanAction{Step: b.StepIndex, Generation: b.PlanGeneration, Agent: req.AgentID, Tool: req.ToolName, Args: redactedArgs(string(req.ArgsJSON))}
	if s.ToolExecutor != nil {
		// The per-target effect record, kept whether or not the request guards are
		// on: coverage settles a write item only by a done write to its target.
		a.Target = s.ToolExecutor.WriteTargets.Key(req.ToolName, req.ArgsJSON)
	}
	if b.Guard != nil {
		// ADR-0145 D14.6: the classes the step's own write items need; coverage does
		// not count them against a constraint.
		a.Carries = append([]domain.ToolEffect(nil), b.Guard.Carries...)
	}
	switch {
	case resp.Denied:
		if !toolKnown {
			return // an unknown tool name never ran and names nothing the caller asked for
		}
		a.Status, a.Detail, a.ApprovalState = domain.PlanActionRefused, resp.DenyReason, resp.ApprovalState
	case resp.Error != "":
		a.Status, a.Detail = domain.PlanActionFailed, resp.Error
	default:
		a.Status = domain.PlanActionDone
		result := resp.ResultJSON
		if len(result) == 0 && domain.IsLedgerID(resp.ResultCID) && s.ledgerOn() {
			// ADR-0145 D1: offloaded as a ledger entry; read back by the kernel for
			// its own action record, acting for the conversation owner (§2.4 bypass
			// with the owner fence kept).
			rctx := domain.WithScope(ctx, runLedgerScope(domain.ConversationOwnerFromContext(ctx)))
			if e, err := s.Ledger.Reader.Get(rctx, resp.ResultCID); err == nil && e != nil {
				result = e.Body
			}
		} else if len(result) == 0 && resp.ResultCID != "" && s.ContentStore != nil {
			// The result was offloaded to the content store; its ids are still wanted.
			if node, err := s.ContentStore.Get(ctx, domain.CID(resp.ResultCID)); err == nil && node != nil {
				result = node.Data
			}
		}
		a.IDs = domain.PlanActionIDs(result)
	}
	s.planActions.record(b.RunID, a)
}

// sideEffectingTool reports (known, effecting) for a tool name: whether the registry
// knows it, and whether a call to it can change the world — a dangerous tool, or one
// declaring an effect other than read/egress (write, spend, admin). Egress alone is a
// fetch, not an action the caller could repeat by mistake. A tool the registry does
// not know (a contributed local:<machine>/<tool>, resolved per call from the
// beneficiary's fleet) is counted as effecting: over-reporting a read costs a line,
// under-reporting a write costs a duplicate.
func (s *Server) sideEffectingTool(name string) (known, effecting bool) {
	if s.ToolExecutor == nil || s.ToolExecutor.Registry == nil {
		return false, true
	}
	t, ok := s.ToolExecutor.Registry.Get(name)
	if !ok {
		return false, strings.HasPrefix(name, domain.LocalToolPrefix)
	}
	return true, t.SideEffecting()
}

// planResultPayload composes the Execute payload for a finished plan: the executor's
// report of its final steps, then the kernel's ledger of side-effecting calls.
func planResultPayload(report string, actions []domain.PlanAction) string {
	ledger := domain.RenderPlanActions(actions)
	switch {
	case ledger == "":
		return report
	case strings.TrimSpace(report) == "":
		return ledger
	}
	return report + "\n\n" + ledger
}

// partialPlanPayload composes the Execute payload for a plan that failed part-way:
// the failure, what the completed steps produced, and the ledger. It used to be the
// error string alone, so a caller learned nothing of the writes that DID happen before
// the failure and re-ordered them. failure is worded as "step N failed: <why>", not
// the PartialPlanError string, which the chat agent's leak filter refuses to speak.
func partialPlanPayload(failure, report string, actions []domain.PlanAction) string {
	var b strings.Builder
	b.WriteString("The plan did not finish: ")
	b.WriteString(failure)
	if strings.TrimSpace(report) != "" {
		b.WriteString("\n\nResults of the steps that completed before it stopped:\n\n")
		b.WriteString(report)
	}
	if ledger := domain.RenderPlanActions(actions); ledger != "" {
		b.WriteString("\n\n")
		b.WriteString(ledger)
	}
	return b.String()
}

// partialFailure words why a partial plan stopped, for partialPlanPayload. A plan
// cut by a limit (idle, max duration, the caller-deadline reserve) ran out of time —
// its step did not fail on its own — and one stopped between steps (FailedStep < 0,
// e.g. cancelled while paused) names no step.
func partialFailure(pe *executer.PartialPlanError, ceiling error) string {
	switch {
	case ceiling != nil && pe.FailedStep >= 0:
		return fmt.Sprintf("it ran out of time at step %d: %v", pe.FailedStep, ceiling)
	case ceiling != nil:
		return fmt.Sprintf("it ran out of time: %v", ceiling)
	case pe.FailedStep < 0:
		return fmt.Sprintf("it was stopped: %v", pe.LastError)
	}
	return fmt.Sprintf("step %d failed: %v", pe.FailedStep, pe.LastError)
}

// planActionsJSON is the ledger as JSON for the response metadata, so a programmatic
// caller need not parse the text; "" when empty.
func planActionsJSON(actions []domain.PlanAction) string {
	if len(actions) == 0 {
		return ""
	}
	type wire struct {
		Step   int               `json:"step"`
		Agent  string            `json:"agent,omitempty"`
		Tool   string            `json:"tool"`
		Status string            `json:"status"`
		Detail string            `json:"detail,omitempty"`
		IDs    []string          `json:"ids,omitempty"`
		Args   map[string]string `json:"args,omitempty"`
	}
	out := make([]wire, len(actions))
	for i, a := range actions {
		out[i] = wire{Step: a.Step, Agent: a.Agent, Tool: a.Tool, Status: a.Status, Detail: a.Detail, IDs: a.IDs, Args: a.Args}
	}
	data, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(data)
}
