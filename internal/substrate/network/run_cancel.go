package network

import (
	"context"
	"log/slog"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// markRunCancelled records an operator-cancelled planner run as cancelled, so
// ResumeRun refuses it (domain.Run.Resumable is false off RunRunning). Contract
// 0102, T-20261106-operator-closesession-only-pauses-a-live-plan. Best effort:
// with no run store, or a run that was never persisted, there is nothing a
// resume could find anyway.
func (s *Server) markRunCancelled(ctx context.Context, runID domain.RunID, cause *operator.RunCancelledError) {
	if s.Runs == nil {
		return
	}
	run, err := s.Runs.GetRun(ctx, runID)
	if err != nil || run == nil {
		slog.Warn("cancelled run not found in the run store; nothing to mark", "run", runID, "err", err)
		return
	}
	run.Status = domain.RunCancelled
	run.EndedAt = time.Now()
	if serr := s.Runs.SaveRun(ctx, *run); serr != nil {
		slog.Warn("could not record the run as cancelled", "run", runID, "err", serr)
		return
	}
	slog.Info("run cancelled by an operator", "run", runID, "actor", cause.Actor, "reason", cause.Reason)
}
