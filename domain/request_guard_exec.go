package domain

import (
	"context"
	"log/slog"
)

// guardOutcome is what the ADR-0145 D14 request guards decided for one call.
type guardOutcome struct {
	// stop: refuse the call with resp.
	stop bool
	resp ToolCallResponse
	// unrequested: a write no open write item asked for; it goes to approval
	// with a notice.
	unrequested bool
	// finish settles the write record with the call's outcome (nil when nothing
	// was reserved or recorded).
	finish func(out ToolCallResponse)
}

// requestGuards runs the D14 guards for a call inside a plan step, in order:
//  1. a checklist constraint that rules out one of the tool's classes refuses it;
//  2. in a step's repair turn, a side-effecting call is refused when the step
//     already executed a write (the repair only corrects the envelope);
//  3. a side-effecting call whose target the request already wrote (or is
//     writing) is refused, whatever its wording;
//  4. a side-effecting call on a step that answers document items and no open
//     write item is refused (a document is satisfied inside Cambrian; only a write
//     item puts anything into an outside system, D14.6);
//  5. a side-effecting call on a step that answers no open write or document item
//     of a checklist is marked unrequested (approval with a notice).
//
// A constraint never refuses a class the step's own write items need
// (StepGuard.Carries, D14.6).
//
// Outside a plan step, or with no guards wired, nothing applies.
func (e *ToolExecutor) requestGuards(ctx context.Context, req ToolCallRequest, tool SystemTool, argHash string) guardOutcome {
	if e.guards == nil {
		return guardOutcome{}
	}
	sc, ok := StepCallFrom(ctx)
	if !ok {
		return guardOutcome{}
	}
	g := sc.Guard
	if c, class, hit := ForbiddenBy(tool, g.Constraints, g.Carries...); hit {
		slog.Info("ADR-0145 D14: call refused by the person's constraint", "tool", req.ToolName,
			"item", c.Item, "class", string(class), "run", sc.RunID, "step", sc.StepIndex)
		return guardOutcome{stop: true, resp: denied(ConstraintRefusal(req.ToolName, c, class), argHash)}
	}
	if !tool.SideEffecting() {
		return guardOutcome{}
	}
	writes := e.guards.Writes
	if g.Repair && writes != nil {
		if done := writes.StepWrites(g.Request, sc.RunID, sc.StepIndex); len(done) > 0 {
			slog.Info("ADR-0145 D14: write refused in a repair turn; the step's write already executed",
				"tool", req.ToolName, "run", sc.RunID, "step", sc.StepIndex)
			return guardOutcome{stop: true, resp: denied(RepairWriteRefusal(req.ToolName, done), argHash)}
		}
	}
	if len(g.WriteItems) == 0 && len(g.Documents) > 0 {
		slog.Info("ADR-0145 D14: outside write refused on a drafting step (a document is finished inside Cambrian)",
			"tool", req.ToolName, "documents", g.Documents, "run", sc.RunID, "step", sc.StepIndex)
		return guardOutcome{stop: true, resp: denied(DocumentWriteRefusal(req.ToolName, g.Documents), argHash)}
	}
	out := guardOutcome{unrequested: g.Checklist && len(g.WriteItems) == 0}
	target := e.guards.Targets.Key(req.ToolName, req.ArgsJSON)
	var release func(ok bool, ids []string, ledgerRef string)
	if writes != nil && target != "" {
		prior, taken, rel := writes.Reserve(g.Request, target, WrittenTarget{Tool: req.ToolName, Run: sc.RunID, Step: sc.StepIndex})
		if taken {
			slog.Info("ADR-0145 D14: write refused; the request already wrote its target",
				"tool", req.ToolName, "target", target, "prior_run", prior.Run, "prior_step", prior.Step)
			return guardOutcome{stop: true, resp: denied(DuplicateTargetRefusal(req.ToolName, prior), argHash)}
		}
		release = rel
	}
	if writes != nil {
		out.finish = func(resp ToolCallResponse) {
			ok := !resp.Denied && resp.Error == ""
			ids := PlanActionIDs(resp.ResultJSON)
			ref := ""
			if IsLedgerID(resp.ResultCID) {
				ref = resp.ResultCID
			}
			if release != nil {
				release(ok, ids, ref)
			}
			if ok {
				writes.RecordStepWrite(g.Request, WrittenTarget{Target: target, Tool: req.ToolName,
					Run: sc.RunID, Step: sc.StepIndex, IDs: ids, LedgerRef: ref})
			}
		}
	}
	return out
}

// refusalIsFinal reports whether a refusal with this approval state will be the
// same if the identical call is made again: every refusal but an approval nobody
// ruled on (pending, expired or withdrawn), which a person may still answer.
func refusalIsFinal(s ApprovalState) bool {
	switch s {
	case ApprovalPending, ApprovalExpired, ApprovalWithdrawn:
		return false
	}
	return true
}

// joinNotices joins approval notices, skipping empty ones.
func joinNotices(parts ...string) string {
	out := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if out != "" {
			out += "\n"
		}
		out += p
	}
	return out
}
