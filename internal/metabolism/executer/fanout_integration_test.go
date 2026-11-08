package executer_test

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

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
		q := plannerQuery(h)
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
		if plannerQuery(h) == "scan" {
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
				q := plannerQuery(h)
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

// The step callback is handed the step as the executor's CURRENT plan has it
// (domain.WithPlanStep), so a fan-out child — which exists only in the expanded
// plan, at an index past the end of the plan the callback was built around — still
// carries the parametric step's routing contract into the auction. Before, the
// server's callback indexed its stale plan, missed, and dispatched every child with
// no capability requirement: a "move each receipt" child reached an agent with no
// move tool (T-20261106-planner-routes-a-file-move-step-to-an-agent).
func TestDAGExecutor_FanOut_ChildrenCarryTheStepContractOnCtx(t *testing.T) {
	dag := &executer.DAGExecutor{MaxFanOutWidth: 10}
	plan := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "list the receipts", RequiredCapabilities: []string{"file_read"}},
		{Query: "move {item} to Receipts", FanOutOver: ptr(0), DependsOn: []int{0},
			RequiredCapabilities: []string{"file_organisation"}, PreferredAgent: "files", AgentPin: "soft"},
	}}
	captured := len(plan.Steps) // what a callback built before the run could index

	var mu sync.Mutex
	got := map[int]domain.Step{}
	stepFn := func(ctx context.Context, i int, h *domain.Handoff) (*domain.Handoff, error) {
		s, ok := domain.PlanStepFrom(ctx)
		if !ok {
			t.Errorf("step %d: no current step on ctx", i)
		}
		// The ctx step is the plan's step (the routing contract); the payload is its
		// query as dispatched, which for a fan-out source carries the JSON-array
		// instruction after it (fanout_source.go).
		if s.Query != plannerQuery(h) {
			t.Errorf("step %d: ctx step %q is not the dispatched step %q", i, s.Query, h.Payload.Data)
		}
		mu.Lock()
		got[i] = s
		mu.Unlock()
		out := "ok"
		if i == 0 {
			out = `["a.pdf","b.pdf"]`
		}
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte(out)}}, nil
	}
	if _, err := dag.Execute(context.Background(), plan, nil, stepFn); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	children := 0
	for i, s := range got {
		if i < captured {
			continue
		}
		children++
		if len(s.RequiredCapabilities) != 1 || s.RequiredCapabilities[0] != "file_organisation" {
			t.Errorf("child %d (%q) lost its required capabilities: %v", i, s.Query, s.RequiredCapabilities)
		}
		if s.PreferredAgent != "files" || s.AgentPin != "soft" {
			t.Errorf("child %d lost its agent pin: %q/%q", i, s.PreferredAgent, s.AgentPin)
		}
	}
	if children != 2 {
		t.Fatalf("want 2 fan-out children past the captured plan, got %d (%v)", children, got)
	}
}

// Two parametric steps fan out over ONE source, and a summary depends on both: every
// child of BOTH runs, and the summary runs after them. Before, only the first parametric
// step over a source was expanded; the sibling and the summary never dispatched
// (T-20261107-only-the-first-fan-out-step-over-a-source).
func TestDAGExecutor_FanOut_SiblingFanOutsOverOneSourceAllRun(t *testing.T) {
	plan := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "aging report"}, // 0
		{Query: "contact for {item}", FanOutOver: ptr(0), DependsOn: []int{0}},  // 1
		{Query: "invoices for {item}", FanOutOver: ptr(0), DependsOn: []int{0}}, // 2
		{Query: "draft reminder for {item}", FanOutOver: ptr(0), DependsOn: []int{1, 2}},
		{Query: "summary", DependsOn: []int{3}}, // 4
	}}
	var mu sync.Mutex
	var order []string
	stepFn := func(_ context.Context, _ int, h *domain.Handoff) (*domain.Handoff, error) {
		q := plannerQuery(h)
		mu.Lock()
		order = append(order, q)
		mu.Unlock()
		out := "ok"
		if q == "aging report" {
			out = `["wychwood","marlowe"]`
		}
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte(out)}}, nil
	}
	if _, err := (&executer.DAGExecutor{MaxFanOutWidth: 10, FanOutConcurrency: 4}).Execute(context.Background(), plan, nil, stepFn); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	pos := map[string]int{}
	for i, q := range order {
		pos[q] = i
	}
	want := []string{"aging report",
		"contact for wychwood", "contact for marlowe",
		"invoices for wychwood", "invoices for marlowe",
		"draft reminder for wychwood", "draft reminder for marlowe", "summary"}
	for _, q := range want {
		if _, ok := pos[q]; !ok {
			t.Errorf("step %q never ran; ran %v", q, order)
		}
	}
	if len(order) != len(want) {
		t.Errorf("ran %d steps, want %d: %v", len(order), len(want), order)
	}
	for _, d := range []string{"draft reminder for wychwood", "draft reminder for marlowe"} {
		for _, up := range want[1:5] {
			if pos[d] < pos[up] {
				t.Errorf("%q ran before its upstream %q: %v", d, up, order)
			}
		}
		if pos["summary"] < pos[d] {
			t.Errorf("summary ran before %q: %v", d, order)
		}
	}
}

// A parametric step with no {item} token runs ONCE over the whole set, never as N
// identical copies of a side-effecting instruction
// (T-20261107-fan-out-step-without-an-item-placeholder).
func TestDAGExecutor_FanOut_UntemplatedStepRunsOnce(t *testing.T) {
	const create = "For each incident lacking an OPS issue, create a new OPS issue"
	plan := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "find incidents"},
		{Query: create, FanOutOver: ptr(0), DependsOn: []int{0}},
		{Query: "report", DependsOn: []int{1}},
	}}
	var mu sync.Mutex
	counts := map[string]int{}
	var createDeps string
	stepFn := func(_ context.Context, _ int, h *domain.Handoff) (*domain.Handoff, error) {
		q := plannerQuery(h)
		mu.Lock()
		counts[q]++
		if q == create {
			createDeps = h.Context[domain.HandoffKeyStepDependencies]
		}
		mu.Unlock()
		out := "ok"
		if q == "find incidents" {
			out = `["INC-1040","INC-1043","INC-1046"]`
		}
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte(out)}}, nil
	}
	if _, err := (&executer.DAGExecutor{MaxFanOutWidth: 10, FanOutConcurrency: 4}).Execute(context.Background(), plan, nil, stepFn); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if counts[create] != 1 {
		t.Fatalf("the untemplated write ran %d times, want exactly 1 (counts %v)", counts[create], counts)
	}
	if counts["report"] != 1 || counts["find incidents"] != 1 || len(counts) != 3 {
		t.Errorf("unexpected executions: %v", counts)
	}
	deps, err := domain.DecodeStepDependencies(createDeps)
	if err != nil || len(deps) != 1 || deps[0].Content == "" {
		t.Errorf("the demoted step must receive the source's whole set; deps = %+v err = %v", deps, err)
	}
}

// Fan-out children run concurrently up to execution.plan.fan_out_concurrency, each on a
// dedicated agent instance; with the bound at 1 (or unset) they run one at a time on
// the shared instance (T-20261107-plan-fan-out-steps-run-one-at-a-time-on-a).
func TestDAGExecutor_FanOut_ChildrenRunConcurrentlyUpToTheBound(t *testing.T) {
	for _, tc := range []struct {
		name          string
		bound         int
		wantPeak      int
		wantDedicated bool
	}{
		{"bound 3", 3, 3, true},
		{"bound 1", 1, 1, false},
		{"unset", 0, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := &domain.ExecutionPlan{Steps: []domain.Step{
				{Query: "list"},
				{Query: "get {item}", FanOutOver: ptr(0), DependsOn: []int{0}},
			}}
			var mu sync.Mutex
			cur, peak, children := 0, 0, 0
			dedicated := map[bool]int{}
			stepFn := func(ctx context.Context, _ int, h *domain.Handoff) (*domain.Handoff, error) {
				q := plannerQuery(h)
				if q == "list" {
					if domain.DedicatedInstanceRequested(ctx) {
						t.Error("an ordinary step must stay on the shared instance")
					}
					return &domain.Handoff{Payload: &domain.Payload{Data: []byte(`["a","b","c","d","e","f","g"]`)}}, nil
				}
				mu.Lock()
				cur++
				children++
				peak = max(peak, cur)
				dedicated[domain.DedicatedInstanceRequested(ctx)]++
				mu.Unlock()
				time.Sleep(30 * time.Millisecond) // long enough for siblings to overlap
				mu.Lock()
				cur--
				mu.Unlock()
				return &domain.Handoff{Payload: &domain.Payload{Data: []byte("ok")}}, nil
			}
			dag := &executer.DAGExecutor{MaxFanOutWidth: 10, FanOutConcurrency: tc.bound}
			if _, err := dag.Execute(context.Background(), plan, nil, stepFn); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if children != 7 {
				t.Fatalf("ran %d children, want 7", children)
			}
			if peak != tc.wantPeak {
				t.Errorf("peak concurrent children = %d, want %d", peak, tc.wantPeak)
			}
			if dedicated[tc.wantDedicated] != 7 {
				t.Errorf("dedicated-instance marker: %v, want all 7 = %v", dedicated, tc.wantDedicated)
			}
		})
	}
}
