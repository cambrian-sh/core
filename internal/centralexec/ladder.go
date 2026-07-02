package centralexec

import (
	"sort"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// ForwardOnlyReplanScope returns the steps a re-plan must revise: the failed
// step plus every step transitively depending on it (forward closure over
// DependsOn). Steps outside this set are still-valid completed results and are
// preserved — a re-plan is minimal and forward-only, never a restart (D3).
func ForwardOnlyReplanScope(steps []domain.Step, failedStepIndex int) []int {
	inScope := map[int]bool{failedStepIndex: true}
	// Iterate to a fixed point: a step joins the scope if any of its
	// dependencies is already in scope (forward propagation of invalidation).
	changed := true
	for changed {
		changed = false
		for i, s := range steps {
			if inScope[i] {
				continue
			}
			for _, dep := range s.DependsOn {
				if inScope[dep] {
					inScope[i] = true
					changed = true
					break
				}
			}
		}
	}
	scope := make([]int, 0, len(inScope))
	for i := range inScope {
		scope = append(scope, i)
	}
	sort.Ints(scope)
	return scope
}

// EscalationAction is one rung of the failure-escalation ladder (ADR-0037 D3).
type EscalationAction int

const (
	// ActionReBind tries the next-best resource for the same intent (plastic, cheap).
	ActionReBind EscalationAction = iota
	// ActionReFrame re-expresses the single intent toward a capability-region
	// that has belief mass (local patch, not skeleton revision).
	ActionReFrame
	// ActionRePlan revises the skeleton — only when a hard failure invalidates a
	// downstream DependsOn precondition (structural, expensive, forward-only).
	ActionRePlan
	// ActionFail reports the task cannot be done (honest terminus).
	ActionFail
)

func (a EscalationAction) String() string {
	switch a {
	case ActionReBind:
		return "re-bind"
	case ActionReFrame:
		return "re-frame"
	case ActionRePlan:
		return "re-plan"
	default:
		return "fail"
	}
}

// StepFailure describes a failed step for the ladder. Hard distinguishes a hard
// failure (no usable output) from a soft/degraded output that merely propagates;
// only a hard failure that invalidated a downstream DependsOn precondition may
// revise the skeleton (D3).
type StepFailure struct {
	StepIndex                       int
	Hard                            bool
	AlternativeResourcesRemain      bool
	Reframable                      bool
	DownstreamDependencyInvalidated bool
}

// LadderState is the bounded recovery budget for one failing step (D3/D15).
type LadderState struct {
	MaxRebinds      int
	RebindAttempts  int
	ReframeBudget   int
	ReframeAttempts int
	// ProgressStalled is the D15 progress guard: a sub-goal that is not a strict
	// refinement of its parent (the livelock signature) trips it, forcing
	// escalation past re-frame rather than spinning.
	ProgressStalled bool
}

// Escalate is the deterministic ladder: it selects the next rung from the
// failure and the recovery state (ADR-0037 D3). The control flow is fixed (the
// safe-path exception to the Zero-Hardcode Rule); the *content* of each rung —
// which resource, how to re-frame, the new skeleton — remains inference. The
// crisp rule: the skeleton is revised iff a hard failure broke a downstream
// DependsOn precondition; a local failure never touches it. The ladder
// terminates on capability or budget exhaustion.
func Escalate(f StepFailure, st LadderState) EscalationAction {
	// Rung 1 — re-bind to the next-best resource for the same intent.
	if f.AlternativeResourcesRemain && st.RebindAttempts < st.MaxRebinds {
		return ActionReBind
	}
	// Rung 2 — re-frame the single intent, bounded by a local budget and the
	// progress guard so it escalates rather than spins.
	if f.Reframable && st.ReframeAttempts < st.ReframeBudget && !st.ProgressStalled {
		return ActionReFrame
	}
	// Rung 3 — revise the skeleton, only on a hard downstream-dependency break.
	if f.Hard && f.DownstreamDependencyInvalidated {
		return ActionRePlan
	}
	// Rung 4 — honest failure (capability/budget exhausted).
	return ActionFail
}
