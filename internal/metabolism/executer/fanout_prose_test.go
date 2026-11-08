package executer_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
)

// o2-incidents-to-issues #3 (run 20261107T103843Z_usecases_full, kernel-U2 13:55:59):
// a thought step wrote markdown, and the write fan-out over it was split into one
// child per line — 15 children, each creating issues for every incident. A
// side-effecting fan-out over a source that is not a structured list must run ONCE
// (T-20261107-fan-out-over-a-prose-source-splits-markdown).
func TestDAGExecutor_FanOut_WriteOverProseRunsOnce(t *testing.T) {
	const prose = "## Comparison Results\n\n# Missing OPS Issues\n\nThe following incidents **do not yet have associated OPS issues**:\n\n" +
		"| Incident ID | Name |\n|---|---|\n| INC-1040 | Temperature excursion |\n| INC-1043 | Label mismatch |\n| INC-1046 | Product stored outside |\n"
	plan := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "read incidents", RequiredEffects: []string{"read"}},
		{Query: "compare", IsThought: true, DependsOn: []int{0}, RequiredEffects: []string{}},
		{Query: "For the incident {item}, create a new OPS issue", DependsOn: []int{1}, FanOutOver: ptr(1), RequiredEffects: []string{"write"}},
	}}
	var mu sync.Mutex
	var writes []string
	stepFn := func(_ context.Context, _ int, h *domain.Handoff) (*domain.Handoff, error) {
		q := plannerQuery(h)
		mu.Lock()
		defer mu.Unlock()
		out := "ok"
		switch {
		case q == "read incidents":
			out = "INC-1040, INC-1043, INC-1046 unresolved"
		case q == "compare":
			out = prose
		case strings.Contains(q, "create a new OPS issue"):
			writes = append(writes, q)
			out = "created OPS-11, OPS-12, OPS-13"
		}
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte(out)}}, nil
	}
	dag := &executer.DAGExecutor{MaxFanOutWidth: 50, FanOutConcurrency: 4}
	got, err := dag.Execute(context.Background(), plan, nil, stepFn)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(writes) != 1 {
		t.Fatalf("the write step ran %d times, want exactly 1: %q", len(writes), writes)
	}
	if !strings.Contains(writes[0], "(each item listed in the result of step 1)") {
		t.Errorf("the demoted write must be pointed at the whole source list; query %q", writes[0])
	}
	if got["_dag_final_result"] != "created OPS-11, OPS-12, OPS-13" {
		t.Errorf("final result = %q", got["_dag_final_result"])
	}
}

// The same plan over a STRUCTURED source still fans out, one write per item.
func TestDAGExecutor_FanOut_WriteOverJSONStillFansOut(t *testing.T) {
	plan := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "compare", IsThought: true, RequiredEffects: []string{}},
		{Query: "For the incident {item}, create a new OPS issue", DependsOn: []int{0}, FanOutOver: ptr(0), RequiredEffects: []string{"write"}},
	}}
	var mu sync.Mutex
	var writes []string
	stepFn := func(_ context.Context, _ int, h *domain.Handoff) (*domain.Handoff, error) {
		q := plannerQuery(h)
		out := "ok"
		if q == "compare" {
			out = "Missing:\n```json\n[\"INC-1040\",\"INC-1043\"]\n```"
		} else {
			mu.Lock()
			writes = append(writes, q)
			mu.Unlock()
		}
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte(out)}}, nil
	}
	if _, err := (&executer.DAGExecutor{MaxFanOutWidth: 50}).Execute(context.Background(), plan, nil, stepFn); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(writes) != 2 {
		t.Fatalf("writes = %q, want one per item", writes)
	}
}
