package domain

import "slices"

// SelfDependencyRepair is one depends_on / fan_out_over entry RepairSelfDependencies
// rewrote: step Step named itself (From) and now names the step before it (To).
type SelfDependencyRepair struct {
	Step  int
	Field string // "depends_on" or "fan_out_over"
	From  int
	To    int
}

// RepairSelfDependencies rewrites the one unambiguous slip of a model numbering
// steps from 1: a step that names ITSELF in depends_on (or fan_out_over) meant the
// step before it.
//
// Evidence (T-20261108-planner-writes-depends-on-1-based-step-n, run
// 20261107T191751Z, g-tr-records-request-1#2): the planner, told depends_on holds
// zero-based indices, wrote [] [] [1] [3] [4] for five steps whose prose said "the
// categorized documents from step 3" — zero-based for step 2, then one-based for
// steps 3 and 4. Validation rejected the self-reference as a cycle, the retry made
// the same plan, and the plan carrying the save step never ran.
//
// Only a self-reference is repaired: no step can depend on itself, and with
// one-based numbering "the previous step" of step i is written i, so i-1 is the
// only reading. Any other index — forward or backward — is a legal edge under
// either numbering, so guessing would rewire a valid plan; it is left alone, and a
// cycle it forms is still the validator's to refuse. A self-reference of step 0
// has no previous step and is left for the validator too. A repaired entry that
// duplicates one already listed is dropped. Returns what was changed, for logging.
func RepairSelfDependencies(plan *ExecutionPlan) []SelfDependencyRepair {
	if plan == nil {
		return nil
	}
	var out []SelfDependencyRepair
	for i := range plan.Steps {
		if i == 0 {
			continue
		}
		st := &plan.Steps[i]
		if slices.Contains(st.DependsOn, i) {
			deps := make([]int, 0, len(st.DependsOn))
			for _, d := range st.DependsOn {
				if d == i {
					d = i - 1
					out = append(out, SelfDependencyRepair{Step: i, Field: "depends_on", From: i, To: i - 1})
				}
				if !slices.Contains(deps, d) {
					deps = append(deps, d)
				}
			}
			st.DependsOn = deps
		}
		if st.FanOutOver != nil && *st.FanOutOver == i {
			prev := i - 1
			st.FanOutOver = &prev
			out = append(out, SelfDependencyRepair{Step: i, Field: "fan_out_over", From: i, To: prev})
			if !slices.Contains(st.DependsOn, prev) {
				st.DependsOn = append(st.DependsOn, prev)
			}
		}
	}
	return out
}
