package executer_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
)

// o2-incidents-to-issues #1 (run 20261107T103843Z_usecases_full, kernel-U2 13:50:23):
// the planner made four INDEPENDENT create-issue steps. All four ran, but the plan's
// result was only the step Kahn's order put last ("OPS-14 has been created"), so the
// chat agent re-delegated the other three and created them twice
// (T-20261107-plan-result-is-only-the-last-topo-step-a-plan).
func TestDAGExecutor_FinalResultReportsEverySinkStep(t *testing.T) {
	incidents := []string{"INC-1040", "INC-1043", "INC-1044", "INC-1046"}
	plan := &domain.ExecutionPlan{}
	for _, inc := range incidents {
		plan.Steps = append(plan.Steps, domain.Step{Query: "Create an OPS issue for " + inc, RequiredEffects: []string{"write"}})
	}
	stepFn := func(_ context.Context, i int, _ *domain.Handoff) (*domain.Handoff, error) {
		out := fmt.Sprintf("OPS issue OPS-%d created for %s", 11+i, incidents[i])
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte(out)}}, nil
	}
	got, err := (&executer.DAGExecutor{}).Execute(context.Background(), plan, nil, stepFn)
	if err != nil {
		t.Fatal(err)
	}
	final := got["_dag_final_result"]
	for i, inc := range incidents {
		if want := fmt.Sprintf("OPS-%d created for %s", 11+i, inc); !strings.Contains(final, want) {
			t.Errorf("final result lacks step %d's result %q:\n%s", i, want, final)
		}
		if want := fmt.Sprintf("### Step %d — Create an OPS issue for %s", i, inc); !strings.Contains(final, want) {
			t.Errorf("final result lacks the label %q", want)
		}
	}
}

// A plan with one sink still returns that sink's result verbatim; an intermediate
// step's result (consumed by the sink) is not repeated.
func TestDAGExecutor_FinalResultSingleSinkIsVerbatim(t *testing.T) {
	plan := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "a"}, {Query: "b"}, {Query: "summary", DependsOn: []int{0, 1}},
	}}
	stepFn := func(_ context.Context, i int, _ *domain.Handoff) (*domain.Handoff, error) {
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte(fmt.Sprintf("r%d", i))}}, nil
	}
	got, err := (&executer.DAGExecutor{}).Execute(context.Background(), plan, nil, stepFn)
	if err != nil {
		t.Fatal(err)
	}
	if got["_dag_final_result"] != "r2" {
		t.Errorf("final = %q, want the summary verbatim", got["_dag_final_result"])
	}
}

// Fan-out children with no reduce step are each a final step, labelled by item; the
// inert parametric node is not.
func TestDAGExecutor_FinalResultReportsEveryFanOutChild(t *testing.T) {
	plan := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "list", RequiredEffects: []string{"read"}},
		{Query: "send a reminder to {item}", FanOutOver: ptr(0), DependsOn: []int{0}, RequiredEffects: []string{"send"}},
	}}
	stepFn := func(_ context.Context, _ int, h *domain.Handoff) (*domain.Handoff, error) {
		q := plannerQuery(h)
		out := "sent: " + q
		if q == "list" {
			out = `["Wychwood","Marlowe","Pentland"]`
		}
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte(out)}}, nil
	}
	got, err := (&executer.DAGExecutor{MaxFanOutWidth: 10}).Execute(context.Background(), plan, nil, stepFn)
	if err != nil {
		t.Fatal(err)
	}
	final := got["_dag_final_result"]
	for _, who := range []string{"Wychwood", "Marlowe", "Pentland"} {
		if !strings.Contains(final, "sent: send a reminder to "+who) || !strings.Contains(final, fmt.Sprintf("(item %q)", who)) {
			t.Errorf("final result lacks %s:\n%s", who, final)
		}
	}
	if strings.Contains(final, "### Step 1 ") {
		t.Errorf("the inert parametric node must not be reported:\n%s", final)
	}
}

// A plan that fails part-way reports what its completed steps produced — the writes
// that DID happen — on the PartialPlanError, not only the failure.
func TestDAGExecutor_PartialPlanReportsCompletedSteps(t *testing.T) {
	plan := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "send to Wychwood"}, {Query: "send to Marlowe"}, {Query: "send to Pentland", DependsOn: []int{0, 1}},
	}}
	stepFn := func(_ context.Context, i int, _ *domain.Handoff) (*domain.Handoff, error) {
		if i == 2 {
			return nil, errors.New("mail server down")
		}
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte(fmt.Sprintf("sent message MSG-%d", i))}}, nil
	}
	_, err := (&executer.DAGExecutor{}).Execute(context.Background(), plan, nil, stepFn)
	var pe *executer.PartialPlanError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *PartialPlanError", err)
	}
	for _, want := range []string{"sent message MSG-0", "sent message MSG-1"} {
		if !strings.Contains(pe.Report, want) {
			t.Errorf("partial report lacks %q:\n%s", want, pe.Report)
		}
	}
}
