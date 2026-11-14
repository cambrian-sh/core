package network

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// parseAuthoredPlan decodes and validates an operator- or plugin-authored plan
// (contract 0074, metadata AuthoredPlanMetadataKey). The operator plane validates
// before accepting, but Execute is the LAST gate before execution and must not
// depend on a caller having done its job; both lanes use this one check, with the
// executor's own TopologicalSort, so their answers cannot disagree.
func parseAuthoredPlan(planJSON string) (*domain.ExecutionPlan, error) {
	var authored domain.ExecutionPlan
	if err := json.Unmarshal([]byte(planJSON), &authored); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "authored plan is not valid JSON: %v", err)
	}
	if _, err := executer.TopologicalSort(authored.Steps); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "authored plan is not executable: %v", err)
	}
	return &authored, nil
}

// authoredPlanBrief renders an authored plan into the single-agent lane's
// request (ENG-1377, owner decision 2026-11-14: the plan is handed to the single
// agent as its brief; never an empty dispatch). The caller's own payload, when
// it says more than the plan's subject (PlanExecutor sends the subject as the
// payload when its caller gave none), leads the brief; then the subject and the
// numbered steps with their order constraints. Step numbers are 1-based for the
// reader; DependsOn indices are 0-based in the plan.
//
// Only what a person reading the plan would need is rendered: the step text, its
// ordering, a reasoning-only mark, a checkpoint question and a per-item step's
// source. Dispatch hints (capabilities, effects, preferred agent) are planner-lane
// selection inputs; in this lane the one agent runs everything and the effect
// gates still apply to every tool call it makes.
//
// A plan with no steps is refused (InvalidArgument) rather than dispatched as a
// request with nothing to do.
func authoredPlanBrief(planJSON, callerInput string) (string, error) {
	plan, err := parseAuthoredPlan(planJSON)
	if err != nil {
		return "", err
	}
	if len(plan.Steps) == 0 {
		return "", status.Error(codes.InvalidArgument, "authored plan has no steps: nothing to run")
	}
	var b strings.Builder
	subject := strings.TrimSpace(plan.Subject)
	if in := strings.TrimSpace(callerInput); in != "" && in != subject {
		b.WriteString(in)
		b.WriteString("\n\n")
	}
	b.WriteString("Carry out this plan. It was written and approved for you; follow its steps in order, " +
		"respecting the stated dependencies. Independent steps may be fanned out with run_subtasks. " +
		"Report the result of each step.\n")
	if subject != "" {
		fmt.Fprintf(&b, "\nPlan: %s\n", subject)
	}
	b.WriteString("\nSteps:\n")
	for i, st := range plan.Steps {
		fmt.Fprintf(&b, "%d. %s", i+1, strings.TrimSpace(st.Query))
		var notes []string
		if len(st.DependsOn) > 0 {
			deps := make([]string, len(st.DependsOn))
			for j, d := range st.DependsOn {
				deps[j] = fmt.Sprint(d + 1)
			}
			notes = append(notes, "after step "+strings.Join(deps, ", "))
		}
		if st.FanOutOver != nil {
			v := st.FanOutVar
			if v == "" {
				v = "item"
			}
			notes = append(notes, fmt.Sprintf("once for each item step %d produced, as {%s}", *st.FanOutOver+1, v))
		}
		if st.IsThought {
			notes = append(notes, "reasoning only, no tools")
		}
		if len(notes) > 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(notes, "; "))
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}
