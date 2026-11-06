package executer

// A replan (InjectCorrection, error replan, hot-swap) carries the swapped-out plan's
// outcomes forward: every step of the replacement plan with no in-plan dependency
// receives them as prior-plan domain.StepDependency entries
// (T-20261106-an-injected-injectcorrection-step-cannot-see).

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

func priorOnly(deps []domain.StepDependency) map[int]domain.StepDependency {
	out := map[int]domain.StepDependency{}
	for _, d := range deps {
		if d.PriorPlan > 0 {
			out[d.Step] = d
		}
	}
	return out
}

// An injected "summarise what you found" step sees the earlier completed results,
// end to end through the real executor, in both workspace modes.
func TestInjectedStep_SeesEarlierCompletedResults(t *testing.T) {
	for _, gw := range []bool{false, true} {
		t.Run(map[bool]string{false: "phase0", true: "global_workspace"}[gw], func(t *testing.T) {
			d := &DAGExecutor{UseGlobalWorkspace: gw, WorkspaceStage: &mockWorkspaceStage{}, ContentStore: newTestStore(t)}
			var mu sync.Mutex
			var summary *domain.Handoff
			injected := false
			stepFn := func(_ context.Context, _ int, h *domain.Handoff) (*domain.Handoff, error) {
				q := string(h.Payload.Data)
				out := map[string]string{"scan": "found ORCHID-4471", "count": "3 files", "report": "never"}[q]
				mu.Lock()
				defer mu.Unlock()
				switch q {
				case "count":
					if !injected {
						injected = true
						if err := d.Inject("summarise what you found"); err != nil {
							t.Errorf("Inject: %v", err)
						}
					}
				case "summarise what you found":
					summary = h
					out = "summary"
				}
				return &domain.Handoff{Payload: &domain.Payload{Data: []byte(out)}}, nil
			}
			plan := &domain.ExecutionPlan{Steps: []domain.Step{
				{Query: "scan"},
				{Query: "count", DependsOn: []int{0}},
				{Query: "report", DependsOn: []int{1}},
			}}
			if _, err := d.Execute(t.Context(), plan, nil, stepFn); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if summary == nil {
				t.Fatal("the injected step never ran")
			}
			deps := decodeManifest(t, summary)
			prior := priorOnly(deps)
			if len(deps) != 2 || len(prior) != 2 {
				t.Fatalf("manifest = %+v", deps)
			}
			for i, want := range map[int]string{0: "found ORCHID-4471", 1: "3 files"} {
				p := prior[i]
				if p.Status != domain.StepDependencyCompleted || p.Content != want || p.Objective != plan.Steps[i].Query || p.PriorPlan != 1 {
					t.Errorf("prior step %d = %+v", i, p)
				}
			}
			if gw && prior[0].CID == "" {
				t.Error("a prior result written to the content store must keep its cid")
			}
		})
	}
}

// replanOnce returns next on the first Replan call.
type replanOnce struct {
	next  *domain.ExecutionPlan
	calls int
}

func (r *replanOnce) Replan(_ context.Context, _ int, _ error, _ map[string]string, _ *domain.ExecutionPlan) (*domain.ExecutionPlan, error) {
	r.calls++
	return r.next, nil
}

// An error replan over a plan with a completed and a failed step: the replacement
// plan's root step gets both, the failure with its error; a replacement step that
// declares an in-plan dependency gets only that.
func TestReplan_MixedCompletedAndFailed_PriorResults(t *testing.T) {
	rh := &replanOnce{next: &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "recover"},
		{Query: "finish", DependsOn: []int{0}},
	}}}
	d := &DAGExecutor{ReplanHandler: rh, MaxReplanAttempts: 1}
	var mu sync.Mutex
	got := map[string]*domain.Handoff{}
	stepFn := func(_ context.Context, _ int, h *domain.Handoff) (*domain.Handoff, error) {
		q := string(h.Payload.Data)
		mu.Lock()
		got[q] = h
		mu.Unlock()
		if q == "write the file" {
			return nil, errors.New("permission denied")
		}
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("out of " + q)}}, nil
	}
	plan := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "read the file"},
		{Query: "write the file", DependsOn: []int{0}},
	}}
	if _, err := d.Execute(t.Context(), plan, nil, stepFn); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if rh.calls != 1 {
		t.Fatalf("replans = %d", rh.calls)
	}
	prior := priorOnly(decodeManifest(t, got["recover"]))
	if p := prior[0]; p.Status != domain.StepDependencyCompleted || p.Content != "out of read the file" {
		t.Errorf("prior step 0 = %+v", p)
	}
	if p := prior[1]; p.Status != domain.StepDependencyFailed || !strings.Contains(p.Error, "permission denied") || p.Objective != "write the file" {
		t.Errorf("prior step 1 = %+v", p)
	}
	fin := decodeManifest(t, got["finish"])
	if len(fin) != 1 || fin[0].PriorPlan != 0 || fin[0].Step != 0 || fin[0].Content != "out of recover" {
		t.Errorf("a step with an in-plan dependency must get only that, got %+v", fin)
	}
}

// Prior results are bounded by the same budget: the largest goes to preview + cid,
// marked shortened; small ones stay whole; failed and empty steps are listed.
func TestPriorPlanResults_BoundedAndListed(t *testing.T) {
	p := &priorPlanResults{}
	plan := &domain.ExecutionPlan{Steps: []domain.Step{{Query: "big"}, {Query: "small"}, {Query: "blank"}, {Query: "boom"}, {Query: "never ran"}}}
	master := map[string]string{
		"step_0_result": strings.Repeat("x", 5000), "step_0_cid": "cid0",
		"step_1_result": "tiny", "step_2_result": "",
	}
	p.capture(master, plan, map[int]bool{0: true, 1: true, 2: true}, &stepFailure{index: 3, err: errors.New("boom failed")})
	sd := p.forRootStep(0, 1000)
	deps, err := domain.DecodeStepDependencies(sd.manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 4 {
		t.Fatalf("want 4 (a step that never ran is not listed), got %+v", deps)
	}
	if !deps[0].Shortened || deps[0].CID != "cid0" || deps[0].Bytes != 5000 || len(deps[0].Content) >= 1000 {
		t.Errorf("big = %+v", deps[0])
	}
	if deps[1].Content != "tiny" || deps[1].Shortened {
		t.Errorf("small = %+v", deps[1])
	}
	if deps[2].Status != domain.StepDependencyEmpty || deps[3].Status != domain.StepDependencyFailed || deps[3].Error != "boom failed" {
		t.Errorf("blank/boom = %+v / %+v", deps[2], deps[3])
	}
	if len(sd.results["prior_1_step_0"]) < 5000 {
		t.Error("cache inputs must hold the full prior result")
	}
	// A second swap accumulates under the next plan number.
	p.capture(map[string]string{"step_0_result": "second"}, &domain.ExecutionPlan{Steps: []domain.Step{{Query: "again"}}}, map[int]bool{0: true}, nil)
	if p.gen != 2 || p.deps[len(p.deps)-1].PriorPlan != 2 {
		t.Errorf("second capture: gen %d, deps %+v", p.gen, p.deps)
	}
}

// A root step's cache key includes the prior-plan results it was handed.
func TestStepCacheKey_IncludesPriorResults(t *testing.T) {
	st := domain.Step{Query: "summarise"}
	a := stepCacheKey("s", "pid", st, map[string]string{"prior_1_step_0": "completed\x00A"})
	b := stepCacheKey("s", "pid", st, map[string]string{"prior_1_step_0": "completed\x00B"})
	none := stepCacheKey("s", "pid", st, nil)
	if a == b || a == none {
		t.Fatal("prior results must change a root step's cache key")
	}
	if a != stepCacheKey("s", "pid", st, map[string]string{"prior_1_step_0": "completed\x00A"}) {
		t.Fatal("key must be deterministic")
	}
}
