package operator

import (
	"errors"
	"fmt"
	"strings"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
)

// PlanIssue is one problem found in an authored plan.
type PlanIssue struct {
	// StepIndex is the offending step, or -1 for a whole-plan problem.
	StepIndex int
	Code      string
	Message   string
	// Fatal distinguishes "this plan cannot run" from "this will probably not do
	// what you meant". Both are worth showing; only one blocks submission.
	// Conflating them either blocks valid plans or runs broken ones.
	Fatal bool
}

// validateAuthoredPlan checks a plan the OPERATOR wrote and returns the issues
// plus the execution order the executor would use.
//
// The cycle and bounds checks delegate to executer.TopologicalSort — the same
// function the executor itself runs. That is deliberate: a second implementation
// could accept a plan the executor then rejects, which would make a passing dry
// run worthless, and it is exactly the class of divergence the UI refactor
// flagged in its own client-side reachability evaluator.
//
// The planner path cannot produce a cycle, so none of this was needed until an
// operator could author a DAG by hand.
func validateAuthoredPlan(steps []domain.Step, knownAgent func(string) bool) ([]PlanIssue, []int) {
	var issues []PlanIssue

	for i, st := range steps {
		if strings.TrimSpace(st.Query) == "" && !st.IsThought {
			issues = append(issues, PlanIssue{
				StepIndex: i, Code: IssueEmptyQuery, Fatal: true,
				Message: "this step has no instruction — an agent would be dispatched with nothing to do",
			})
		}

		// A pin on an agent that does not exist is worth flagging BEFORE it
		// strands the step at runtime, and the two pin strengths fail
		// differently: a hard pin kills the step, a soft one cascades.
		if st.PreferredAgent != "" && knownAgent != nil && !knownAgent(st.PreferredAgent) {
			hard := strings.EqualFold(st.AgentPin, domain.PinHard)
			msg := fmt.Sprintf("no agent %q is registered; this step will fall back to ordinary selection", st.PreferredAgent)
			if hard {
				msg = fmt.Sprintf("no agent %q is registered, and this is a HARD pin — the step will fail rather than fall back", st.PreferredAgent)
			}
			issues = append(issues, PlanIssue{
				StepIndex: i, Code: IssueUnknownAgent, Message: msg, Fatal: hard,
			})
		}

		if st.FanOutOver != nil {
			fo := *st.FanOutOver
			switch {
			case fo < 0 || fo >= len(steps):
				issues = append(issues, PlanIssue{
					StepIndex: i, Code: IssueBadFanOut, Fatal: true,
					Message: fmt.Sprintf("fan-out source step %d does not exist", fo),
				})
			case fo == i:
				issues = append(issues, PlanIssue{
					StepIndex: i, Code: IssueBadFanOut, Fatal: true,
					Message: "a step cannot fan out over its own output",
				})
			case !dependsOn(st, fo):
				// Not fatal — the executor would still run it — but it is almost
				// certainly a mistake: the source's output has to EXIST before
				// this step can expand over it, and only a dependency guarantees
				// that ordering.
				issues = append(issues, PlanIssue{
					StepIndex: i, Code: IssueBadFanOut, Fatal: false,
					Message: fmt.Sprintf("this step fans out over step %d but does not depend on it, so it may run before that output exists", fo),
				})
			}
		}
	}

	order, err := executer.TopologicalSort(steps)
	if err != nil {
		var cyc *executer.CyclicPlanError
		code := IssueOutOfBoundsDep
		if errors.As(err, &cyc) {
			code = IssueCycle
		}
		issues = append(issues, PlanIssue{
			StepIndex: -1, Code: code, Message: err.Error(), Fatal: true,
		})
		return issues, nil
	}
	return issues, order
}

// dependsOn reports whether step lists idx among its dependencies.
func dependsOn(step domain.Step, idx int) bool {
	for _, d := range step.DependsOn {
		if d == idx {
			return true
		}
	}
	return false
}

// planSummary renders an authored plan for the audit record: enough to see what
// was submitted without embedding whole prompts in the log.
func planSummary(steps []domain.Step) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d steps", len(steps))
	for i, st := range steps {
		if i >= 5 {
			fmt.Fprintf(&b, "; …+%d more", len(steps)-i)
			break
		}
		q := st.Query
		if len(q) > 60 {
			q = q[:60] + "…"
		}
		fmt.Fprintf(&b, "; [%d] %s", i, q)
	}
	return b.String()
}
