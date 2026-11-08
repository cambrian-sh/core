package executer

import (
	"fmt"
	"sort"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// reportQueryRunes bounds a step's objective in a labelled report section.
const reportQueryRunes = 160

// buildPlanReport is the plan's result for its caller: the output of every FINAL step
// among the completed ones — a completed step no other completed step consumed —
// rather than only the step Kahn's order happens to put last
// (T-20261107-plan-result-is-only-the-last-topo-step-a-plan). A plan of four
// independent "create issue" steps has four final steps; returning one of them told
// the chat agent three issues had not been created, and it created them again.
//
// For a finished plan the final steps are its sinks; for a plan that stopped
// part-way they are the completed frontier. An inert fan-out node (still FanOutOver-
// tagged) is never reported — its children are. One final step ⇒ its result verbatim
// (the shape every single-sink plan has always returned); several ⇒ one labelled
// section each, in step order, so every result reaches the caller.
func buildPlanReport(plan *domain.ExecutionPlan, master map[string]string, completed map[int]bool) string {
	if plan == nil {
		return ""
	}
	reportable := func(i int) bool {
		return i >= 0 && i < len(plan.Steps) && completed[i] && plan.Steps[i].FanOutOver == nil
	}
	consumed := map[int]bool{}
	for j := range plan.Steps {
		if !reportable(j) {
			continue
		}
		for _, d := range plan.Steps[j].DependsOn {
			consumed[d] = true
		}
	}
	var finals []int
	for i := range plan.Steps {
		if reportable(i) && !consumed[i] {
			finals = append(finals, i)
		}
	}
	sort.Ints(finals)
	switch len(finals) {
	case 0:
		return ""
	case 1:
		return master[fmt.Sprintf("step_%d_result", finals[0])]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "This plan has %d final steps; the result of each follows.", len(finals))
	for _, i := range finals {
		st := plan.Steps[i]
		label := fmt.Sprintf("Step %d", i)
		if st.FanOutItem != "" {
			label += fmt.Sprintf(" (item %q)", boundRunes(st.FanOutItem, reportQueryRunes))
		}
		res := strings.TrimSpace(master[fmt.Sprintf("step_%d_result", i)])
		if res == "" {
			res = "(no output)"
		}
		fmt.Fprintf(&b, "\n\n### %s — %s\n%s", label, boundRunes(oneLineQuery(st.Query), reportQueryRunes), res)
	}
	return b.String()
}

func oneLineQuery(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
