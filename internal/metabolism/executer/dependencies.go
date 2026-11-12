package executer

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// stepDeps is what the coordinator hands a dispatched step about its upstream steps.
type stepDeps struct {
	// manifest is the encoded []domain.StepDependency set on the step's handoff
	// metadata under domain.HandoffKeyStepDependencies; "" for a root step.
	manifest string
	// results are the FULL upstream results keyed step_N_result, for the step cache
	// key: the manifest is bounded, the cache key must not be (two results that share
	// a preview are still different inputs).
	results map[string]string
	// refs are the ledger ids of the completed upstream outputs (ADR-0145 D6: a
	// step may cite its inputs).
	refs []string
}

// buildStepDependencies lists the results of step's DependsOn steps, in DependsOn
// order, from the coordinator's masterContext. Caller holds the context lock.
//
// It is the ONE place a dependent step's upstream results are assembled, for every
// executor (agent or in-kernel thought step) and both workspace modes, so a step
// never depends on which transport happened to carry them
// (T-20261105-a-planner-thought-step-that-depends-on-two). Each dependency carries
// its index, objective (the upstream query), status and content; an empty or
// unrecorded result is listed with that status rather than omitted. Content is
// bounded to fit across the set (domain.FitStepDependencies): a shortened result
// keeps its beginning, its end, its outline and the exact read call for the rest.
//
// A step with no in-plan DependsOn receives the PRIOR-PLAN results instead, when a
// replan has swapped a plan out of this run (prior; see priorPlanResults): an injected
// "summarise what you found" step, or any root step of a replanned plan, is otherwise
// blind to everything the run already did.
func buildStepDependencies(master map[string]string, plan *domain.ExecutionPlan, stepIdx int, fit domain.DependencyFit, prior *priorPlanResults) stepDeps {
	return buildStepDependenciesFrom(master, plan, stepIdx, fit, prior, nil)
}

// dependencyResolver reads a completed upstream step's result by its reference
// (step_N_cid). With the run ledger on (ADR-0145 D1) it is the ledger read, and
// it is the ONLY source of a dependant's content: the masterContext string is a
// view, not the source of truth. nil means the legacy path (masterContext content).
type dependencyResolver func(ref string) (string, error)

// buildStepDependenciesFrom is buildStepDependencies with the content of each
// completed dependency read through resolve when it is non-nil. A dependency the
// resolver cannot read is listed as unavailable with the reason, never silently
// replaced by the in-memory string.
func buildStepDependenciesFrom(master map[string]string, plan *domain.ExecutionPlan, stepIdx int, fit domain.DependencyFit, prior *priorPlanResults, resolve dependencyResolver) stepDeps {
	step := plan.Steps[stepIdx]
	if len(step.DependsOn) == 0 {
		return prior.forRootStep(stepIdx, fit)
	}
	deps := make([]domain.StepDependency, 0, len(step.DependsOn))
	results := make(map[string]string, len(step.DependsOn))
	var refs []string
	for _, idx := range step.DependsOn {
		dep := domain.StepDependency{Step: idx}
		if idx >= 0 && idx < len(plan.Steps) {
			dep.Objective = plan.Steps[idx].Query
		}
		key := fmt.Sprintf("step_%d_result", idx)
		res, ok := master[key]
		switch {
		case !ok:
			dep.Status = domain.StepDependencyUnavailable
		case strings.TrimSpace(res) == "":
			dep.Status = domain.StepDependencyEmpty
		default:
			dep.Status = domain.StepDependencyCompleted
			dep.Content = res
			dep.Bytes = len(res)
			dep.CID = master[fmt.Sprintf("step_%d_cid", idx)]
			if domain.IsLedgerID(dep.CID) {
				refs = append(refs, dep.CID)
			}
			if resolve != nil {
				content, err := resolve(dep.CID)
				if err != nil {
					dep.Status = domain.StepDependencyUnavailable
					dep.Content, dep.Bytes = "", 0
					dep.Error = err.Error()
					slog.Warn("ADR-0145: upstream result not readable from the ledger", "step", stepIdx, "dependency", idx, "err", err)
				} else {
					dep.Content, dep.Bytes = content, len(content)
					res = content
				}
			}
		}
		results[key] = dep.Status + "\x00" + res
		deps = append(deps, dep)
	}
	deps = domain.FitStepDependencies(deps, fit)
	manifest, err := domain.EncodeStepDependencies(deps)
	if err != nil {
		// A []StepDependency of strings and ints always marshals; logged, not
		// swallowed, should that ever change.
		slog.Error("step dependencies not encoded; the step runs without its upstream results",
			"step", stepIdx, "err", err)
	}
	return stepDeps{manifest: manifest, results: results, refs: refs}
}

// stepFailure is the failed step of a plan being swapped out, if any.
type stepFailure struct {
	index int
	err   error
}

// failureOf is nil when err is nil.
func failureOf(err error, index int) *stepFailure {
	if err == nil {
		return nil
	}
	return &stepFailure{index: index, err: err}
}

// priorPlanResults accumulates, across the replans of one Execute, the outcomes of
// every plan swapped out of the run: each completed step's result (full, bounded only
// when handed to a step) and the failed step with its error. Coordinator-owned.
//
// It exists because applyReplannedPlan resets the completed set and the replacement
// plan reuses step indices, so the swapped-out plan's step_N_result keys are both
// unreachable (no new step declares a dependency on them) and about to be
// overwritten. Capturing them at the swap is the only point where "what did the run
// already do" is still knowable (T-20261106-an-injected-injectcorrection-step-cannot-see).
type priorPlanResults struct {
	gen  int
	deps []domain.StepDependency
}

// capture records the outcome of plan, which is about to be swapped out. Caller holds
// the context lock. Steps that never ran are not listed; a fan-out's inert node is
// listed only if its result was recorded (an empty fan-out), its children carry the rest.
func (p *priorPlanResults) capture(master map[string]string, plan *domain.ExecutionPlan, completed map[int]bool, fail *stepFailure) {
	if p == nil || plan == nil {
		return
	}
	gen := p.gen + 1
	added := false
	for i, st := range plan.Steps {
		dep := domain.StepDependency{Step: i, Objective: st.Query, PriorPlan: gen}
		switch {
		case fail != nil && fail.index == i:
			dep.Status = domain.StepDependencyFailed
			dep.Error = fail.err.Error()
		case completed[i]:
			res, ok := master[fmt.Sprintf("step_%d_result", i)]
			switch {
			case !ok && st.FanOutOver != nil:
				continue
			case !ok:
				dep.Status = domain.StepDependencyUnavailable
			case strings.TrimSpace(res) == "":
				dep.Status = domain.StepDependencyEmpty
			default:
				dep.Status = domain.StepDependencyCompleted
				dep.Content = res
				dep.Bytes = len(res)
				dep.CID = master[fmt.Sprintf("step_%d_cid", i)]
			}
		default:
			continue
		}
		p.deps = append(p.deps, dep)
		added = true
	}
	if added {
		p.gen = gen
	}
}

// forRootStep is the manifest for a step with no in-plan dependency: every prior-plan
// outcome, oldest plan first, bounded to fit (domain.FitStepDependencies: the
// largest results are shortened first, each to beginning + end + outline). Empty when no
// plan has been swapped out.
func (p *priorPlanResults) forRootStep(stepIdx int, fit domain.DependencyFit) stepDeps {
	if p == nil || len(p.deps) == 0 {
		return stepDeps{}
	}
	deps := append([]domain.StepDependency(nil), p.deps...)
	results := make(map[string]string, len(deps))
	for _, d := range deps {
		// Full content, not the bounded preview: the step cache must key on it.
		results[fmt.Sprintf("prior_%d_step_%d", d.PriorPlan, d.Step)] = d.Status + "\x00" + d.Content + "\x00" + d.Error
	}
	deps = domain.FitStepDependencies(deps, fit)
	manifest, err := domain.EncodeStepDependencies(deps)
	if err != nil {
		slog.Error("prior plan results not encoded; the step runs without them", "step", stepIdx, "err", err)
	}
	return stepDeps{manifest: manifest, results: results}
}
