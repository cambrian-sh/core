package executer_test

import (
	"context"
	"sort"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/metabolism/executer"
)

func ptr(i int) *int { return &i }

// End-to-end: a parametric fan-out node expands over a prior step's discovered set and its
// children actually execute, with a reduce step barriering over them — no replan, no LLM.
func TestDAGExecutor_FanOut_ExpandsAndRunsChildren(t *testing.T) {
	dag := &executer.DAGExecutor{MaxFanOutWidth: 10}

	plan := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "scan"}, // 0 — discovery
		{Query: "write {item}", FanOutOver: ptr(0), DependsOn: []int{0}}, // 1 — parametric
		{Query: "reduce", DependsOn: []int{1}},                           // 2 — barrier
	}}

	var mu sync.Mutex
	ran := []string{}
	stepFn := func(_ context.Context, _ int, h *domain.Handoff) (*domain.Handoff, error) {
		q := string(h.Payload.Data)
		mu.Lock()
		ran = append(ran, q)
		mu.Unlock()
		out := "ok"
		if q == "scan" {
			out = `["a","b","c"]` // discovery reveals three items
		}
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte(out)}}, nil
	}

	if _, err := dag.Execute(context.Background(), plan, nil, stepFn); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	mu.Lock()
	got := append([]string(nil), ran...)
	mu.Unlock()
	sort.Strings(got)

	has := func(q string) bool {
		for _, r := range got {
			if r == q {
				return true
			}
		}
		return false
	}

	for _, want := range []string{"scan", "write a", "write b", "write c", "reduce"} {
		if !has(want) {
			t.Errorf("expected step %q to run; ran = %v", want, got)
		}
	}
	if has("write {item}") {
		t.Errorf("the inert parametric node must never execute; ran = %v", got)
	}
	if len(got) != 5 {
		t.Errorf("expected exactly 5 executions (scan + 3 children + reduce), got %d: %v", len(got), got)
	}
}

// Over-width fan-out with no ReplanHandler surfaces as a hard error, never a silent
// truncation that drops items the user asked for.
func TestDAGExecutor_FanOut_OverWidthErrors(t *testing.T) {
	dag := &executer.DAGExecutor{MaxFanOutWidth: 2} // cap below the discovered set

	plan := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "scan"},
		{Query: "write {item}", FanOutOver: ptr(0), DependsOn: []int{0}},
	}}
	stepFn := func(_ context.Context, _ int, h *domain.Handoff) (*domain.Handoff, error) {
		out := "ok"
		if string(h.Payload.Data) == "scan" {
			out = `["a","b","c"]` // 3 > cap of 2
		}
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte(out)}}, nil
	}

	_, err := dag.Execute(context.Background(), plan, nil, stepFn)
	if err == nil {
		t.Fatal("expected an error when fan-out exceeds the width cap, got nil")
	}
}

// The reduce step sees every fan-out child's result, by step and objective; a fan-out
// that found nothing is reported to it as an EMPTY upstream step, not dropped
// (T-20261105-a-planner-thought-step-that-depends-on-two).
func TestDAGExecutor_FanOut_ReduceReceivesChildResults(t *testing.T) {
	for _, tc := range []struct {
		name, discovered string
		want             map[string]string // objective -> status
	}{
		{"three", `["a","b","c"]`, map[string]string{"write a": "completed", "write b": "completed", "write c": "completed"}},
		{"none", `[]`, map[string]string{"write {item}": "empty"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := &domain.ExecutionPlan{Steps: []domain.Step{
				{Query: "scan"},
				{Query: "write {item}", FanOutOver: ptr(0), DependsOn: []int{0}},
				{Query: "reduce", DependsOn: []int{1}},
			}}
			var mu sync.Mutex
			var reduce *domain.Handoff
			stepFn := func(_ context.Context, _ int, h *domain.Handoff) (*domain.Handoff, error) {
				q := string(h.Payload.Data)
				out := "done " + q
				switch q {
				case "scan":
					out = tc.discovered
				case "reduce":
					mu.Lock()
					reduce = h
					mu.Unlock()
				}
				return &domain.Handoff{Payload: &domain.Payload{Data: []byte(out)}}, nil
			}
			if _, err := (&executer.DAGExecutor{MaxFanOutWidth: 10}).Execute(context.Background(), plan, nil, stepFn); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if reduce == nil {
				t.Fatal("reduce never ran")
			}
			deps, err := domain.DecodeStepDependencies(reduce.Context[domain.HandoffKeyStepDependencies])
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]string{}
			for _, d := range deps {
				got[d.Objective] = d.Status
				if d.Status == domain.StepDependencyCompleted && d.Content != "done "+d.Objective {
					t.Errorf("child %q content = %q", d.Objective, d.Content)
				}
			}
			if len(got) != len(tc.want) {
				t.Fatalf("deps = %+v, want %v", deps, tc.want)
			}
			for obj, st := range tc.want {
				if got[obj] != st {
					t.Errorf("%q: status %q, want %q (deps %+v)", obj, got[obj], st, deps)
				}
			}
		})
	}
}
