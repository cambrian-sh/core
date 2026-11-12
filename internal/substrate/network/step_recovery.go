package network

import (
	"errors"
	"fmt"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
)

// stageReplan is the note the next coverage stage's planner gets when stage
// `stage` stopped at a step whose failure needs re-planning (a missing capability,
// a repeated identical tool failure; domain.StepFailureNeedsReplan). Under the
// checklist there is no mid-stage replan (stageStoppedPartway): the next stage's
// planner IS the re-planner, so the failure goes to it with the error — which
// names what the step lacked — and a stage that changed nothing because of it is
// not a stall (runStages; owner decision 2026-11-12 on
// T-20261112-yield-sub-goal-on-a-composed-lease-runs-under: capability_missing
// "goes straight to the re-planner, which re-plans that step"). The note is data
// in the stage block's earlier-stages region, never rule text. "" for any other
// failure, and for a stage that did not stop.
func stageReplan(err error, stage int) string {
	if err == nil || domain.ClassOfStepError(err) != domain.StepFailureNeedsReplan {
		return ""
	}
	var pe *executer.PartialPlanError
	if !errors.As(err, &pe) {
		return ""
	}
	return fmt.Sprintf("Stage %d stopped at step %d, which was not done and needs re-planning: %v",
		stage, pe.FailedStep, pe.LastError)
}
