package app

import (
	"log/slog"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/metabolism/dispatch"
	subnetwork "github.com/cambrian-sh/core/internal/substrate/network"
)

// wireStepRecovery threads execution.tools.repeated_failure_stop to its two
// consumers in one place (T-20261112-yield-sub-goal-on-a-composed-lease-runs-under):
// the ToolExecutor counts each step lease's identical failed calls and stops the
// step at the threshold, and the Dispatcher ends a stopped step with the
// domain.StepStuckError (class needs_replan) so it goes to the re-planner. Off
// (0 or 1) ⇒ nothing is touched.
func wireStepRecovery(cfg config.ExecutionConfig, server *subnetwork.Server, d *dispatch.Dispatcher) {
	f := domain.NewStepFailures(cfg.Tools.RepeatedFailureStop)
	if f == nil {
		slog.Info("step recovery: repeated identical tool failures do not stop a step (execution.tools.repeated_failure_stop off)")
		return
	}
	if server == nil || server.ToolExecutor == nil || d == nil {
		slog.Error("step recovery: execution.tools.repeated_failure_stop is set but the tool executor or dispatcher is missing; it is OFF")
		return
	}
	server.ToolExecutor.StepFailures = f
	d.StepFailures = f
	slog.Info("step recovery: a step stops after identical tool failures (execution.tools.repeated_failure_stop)",
		"threshold", f.Threshold())
}
