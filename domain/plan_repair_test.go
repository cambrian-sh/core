package domain

import (
	"reflect"
	"testing"
)

// The recorded plan of g-tr-records-request-1#2 (run 20261107T191751Z, kernel-U17
// 00:23:14Z): depends_on [] [] [1] [3] [4] — steps 3 and 4 numbered one-based.
func TestRepairSelfDependencies_RecordedPlan(t *testing.T) {
	t.Parallel()
	plan := &ExecutionPlan{Steps: []Step{
		{Query: "Retrieve the case file BE-2026/0001"},
		{Query: "Search the records registry"},
		{Query: "Categorise the documents found", DependsOn: []int{1}},
		{Query: "Based on the categorized documents from step 3, prepare a response draft", DependsOn: []int{3}},
		{Query: "Record the response with cases_respond_request: the draft prepared in step 4", DependsOn: []int{4}},
	}}
	got := RepairSelfDependencies(plan)
	want := []SelfDependencyRepair{{3, "depends_on", 3, 2}, {4, "depends_on", 4, 3}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("repairs = %+v, want %+v", got, want)
	}
	for i, s := range plan.Steps {
		for _, d := range s.DependsOn {
			if d >= i {
				t.Fatalf("step %d still depends on %d", i, d)
			}
		}
	}
	if !reflect.DeepEqual(plan.Steps[3].DependsOn, []int{2}) || !reflect.DeepEqual(plan.Steps[4].DependsOn, []int{3}) {
		t.Errorf("deps = %v %v", plan.Steps[3].DependsOn, plan.Steps[4].DependsOn)
	}
}

func TestRepairSelfDependencies_LeavesEverythingElse(t *testing.T) {
	t.Parallel()
	plan := &ExecutionPlan{Steps: []Step{
		{Query: "a", DependsOn: []int{0}}, // step 0 has no previous step: the validator's
		{Query: "b", DependsOn: []int{2}}, // a forward edge is legal: untouched
		{Query: "c"},
		{Query: "d", DependsOn: []int{2, 3}, FanOutOver: iptr(3)}, // self + duplicate
	}}
	got := RepairSelfDependencies(plan)
	if len(got) != 2 {
		t.Fatalf("repairs = %+v", got)
	}
	if !reflect.DeepEqual(plan.Steps[0].DependsOn, []int{0}) || !reflect.DeepEqual(plan.Steps[1].DependsOn, []int{2}) {
		t.Errorf("unambiguous-only repair changed %v %v", plan.Steps[0].DependsOn, plan.Steps[1].DependsOn)
	}
	if !reflect.DeepEqual(plan.Steps[3].DependsOn, []int{2}) || *plan.Steps[3].FanOutOver != 2 {
		t.Errorf("step 3 = deps %v fan %d", plan.Steps[3].DependsOn, *plan.Steps[3].FanOutOver)
	}
	if RepairSelfDependencies(nil) != nil {
		t.Error("nil plan")
	}
}
