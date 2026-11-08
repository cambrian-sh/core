package awareness

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// T-20261108-planner-writes-depends-on-1-based-step-n: the recorded reply wrote
// depends_on [3] on step 3 and [4] on step 4; the planner repairs the self-reference
// to the previous step instead of handing the validator a cycle, and its prompt now
// states the numbering.
func TestPlanner_RepairsOneBasedSelfDependencies(t *testing.T) {
	gen := &requestRecorder{reply: `{"reasoning":"r","subject":"FOI","cache_policy":"tool","steps":[
	 {"query":"Retrieve the case file","depends_on":[]},
	 {"query":"Search the registry","depends_on":[]},
	 {"query":"Categorise the documents","depends_on":[1]},
	 {"query":"Based on the categorized documents from step 3, draft the response","depends_on":[3]},
	 {"query":"Record the response drafted in step 4","depends_on":[4]}]}`}
	p := NewPlanner(gen, &vocabAgents{agents: goldenAgents()}, nil)
	p.goos = "linux"
	p.SetCalendar(goldenCalendar())
	plan, err := p.GetExecutionPlan(context.Background(), "respond to the FOI request")
	if err != nil {
		t.Fatal(err)
	}
	got := [][]int{plan.Steps[2].DependsOn, plan.Steps[3].DependsOn, plan.Steps[4].DependsOn}
	if !reflect.DeepEqual(got, [][]int{{1}, {2}, {3}}) {
		t.Fatalf("deps = %v", got)
	}
	if sys := gen.only(t).System[0].Text; !strings.Contains(sys, "Steps are numbered from 0: the first step is step 0") {
		t.Error("the planner prompt does not state the step numbering")
	}
}
