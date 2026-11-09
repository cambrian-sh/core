package network

import (
	"context"
	"log/slog"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// The server's half of the ADR-0145 D14 request-tied guards
// (execution.tools.request_guards; T-20261109-adr-0145-d14-fix-batch-remove-per-step):
// the facts the ToolExecutor's guards need about the request a plan step works
// for are computed here, at dispatch, from the kernel's own state (the run's
// requirement checklist and its coverage, the ordering conversation turn) and
// bound to the step's lease. Nothing comes from an agent.

// requestGuardsOn reports whether the D14 guards run.
func (s *Server) requestGuardsOn() bool {
	return s.ExecCfg.Tools.RequestGuards && s.ToolExecutor != nil && s.ToolExecutor.Guards != nil
}

// requestScope is the scope of the per-target write record: the ordering
// conversation turn when the run's ledger provenance names one, else the root
// run (every coverage stage and plan generation of one Execute shares it).
func requestScope(ctx context.Context, executionID domain.RunID) string {
	if p := domain.LedgerProvenanceFromContext(ctx); p.ConversationID != "" && p.TurnID != "" {
		return "turn:" + p.ConversationID + "/" + p.TurnID
	}
	if executionID == "" {
		return ""
	}
	return "run:" + string(executionID)
}

// stepGuard computes a plan step's StepGuard (nil when the guards are off).
func (s *Server) stepGuard(ctx context.Context, step domain.Step, reqs *requestRequirements, executionID domain.RunID) *domain.StepGuard {
	if !s.requestGuardsOn() {
		return nil
	}
	g := &domain.StepGuard{Request: requestScope(ctx, executionID), Repair: domain.RepairTurnFrom(ctx)}
	if reqs == nil || reqs.checklist == nil {
		return g
	}
	g.Checklist = true
	terms, err := s.ExecCfg.Tools.ConstraintTerms()
	if err != nil {
		slog.Error("ADR-0145 D14: execution.tools.constraint_classes invalid; checklist constraints are NOT enforced", "err", err)
	} else {
		g.Constraints = domain.ResolveConstraints(reqs.checklist, terms)
	}
	for _, id := range step.Answers {
		it := reqs.checklist.Item(id)
		if it == nil || (it.Type != domain.RequirementWrite && it.Type != domain.RequirementDocument) {
			continue
		}
		if reqs.coverage != nil && reqs.coverage.Status(id).Final() {
			continue
		}
		g.WriteItems = append(g.WriteItems, id)
	}
	return g
}

// writtenTargets lists the targets the request has written, for the planner and
// the validator ("<tool> → <target>").
func (s *Server) writtenTargets(ctx context.Context, executionID domain.RunID) []domain.WrittenTarget {
	if !s.requestGuardsOn() {
		return nil
	}
	return s.ToolExecutor.Guards.Writes.Targets(requestScope(ctx, executionID))
}

// renderWrittenTargets is the planner-facing list of written targets.
func renderWrittenTargets(ts []domain.WrittenTarget) string {
	var b strings.Builder
	for _, t := range ts {
		b.WriteString("- " + t.Tool + " → " + domain.TargetValue(t.Target))
		if len(t.IDs) > 0 {
			b.WriteString(" (" + strings.Join(t.IDs, ", ") + ")")
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
