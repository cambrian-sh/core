package network

import (
	"encoding/json"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
)

// stampExecuteResult builds the structured Execute result (ADR-0145 D8) and puts
// it on the response metadata under domain.HandoffKeyExecuteResult, consuming the
// executor's internal step index (executer.PlanStepsKey). The payload keeps the
// report text unchanged, so a caller that reads only text sees no difference.
func (s *Server) stampExecuteResult(meta map[string]string, report string, run domain.RunID, sid domain.SessionID,
	actions []domain.PlanAction, partial *executer.PartialPlanError, ceiling error) {
	if meta == nil {
		return
	}
	res := domain.ExecuteResult{
		Version:    domain.ExecuteResultVersion,
		RunID:      string(run),
		SessionID:  string(sid),
		Report:     report,
		Actions:    domain.ExecuteActionsOf(actions),
		Steps:      executer.PlanStepRefsFrom(meta),
		FailedStep: -1,
		Ledger:     s.ledgerOn(),
	}
	delete(meta, executer.PlanStepsKey)
	if res.Steps == nil {
		res.Steps = []domain.ExecuteStepRef{}
	}
	if partial != nil {
		res.Partial = true
		res.FailedStep = partial.FailedStep
		if partial.LastError != nil {
			res.Error = partial.LastError.Error()
		}
	}
	if ceiling != nil {
		res.TimedOut = true
		res.TimeoutCause = ceiling.Error()
	}
	b, err := json.Marshal(res)
	if err != nil {
		slog.Warn("ADR-0145: structured Execute result not encoded", "run", run, "err", err)
		return
	}
	meta[domain.HandoffKeyExecuteResult] = string(b)
}
