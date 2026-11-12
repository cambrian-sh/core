package executer

// Upstream results reach every dependent step as a structured domain.StepDependency
// manifest, whichever executor runs it and in both workspace modes
// (T-20261105-a-planner-thought-step-that-depends-on-two).

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// mergePlan is the E42 shape: two independent reads, then a thought step merging them.
func mergePlan() *domain.ExecutionPlan {
	return &domain.ExecutionPlan{Subject: "read two files", Steps: []domain.Step{
		{Query: "read a.txt"},
		{Query: "read b.txt"},
		{Query: "Merge: A=<step 0> B=<step 1>", IsThought: true, DependsOn: []int{0, 1}},
	}}
}

func decodeManifest(t *testing.T, h *domain.Handoff) []domain.StepDependency {
	t.Helper()
	deps, err := domain.DecodeStepDependencies(h.Context[domain.HandoffKeyStepDependencies])
	if err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	return deps
}

// The merge step — run by ThoughtFn — receives both upstream results, in DependsOn
// order, with index, objective and status, in BOTH workspace modes.
func TestDependentThoughtStep_ReceivesBothUpstreamResults(t *testing.T) {
	for _, gw := range []bool{false, true} {
		t.Run(map[bool]string{false: "phase0", true: "global_workspace"}[gw], func(t *testing.T) {
			var got *domain.Handoff
			ex := &DAGExecutor{
				UseGlobalWorkspace: gw,
				WorkspaceStage:     &mockWorkspaceStage{},
				ContentStore:       newTestStore(t),
				ThoughtFn: func(_ context.Context, _ int, h *domain.Handoff) (*domain.Handoff, error) {
					got = h
					return &domain.Handoff{Payload: &domain.Payload{Data: []byte("A=ORCHID-4471\nB=CANYON-9026")}}, nil
				},
			}
			agent := func(_ context.Context, i int, _ *domain.Handoff) (*domain.Handoff, error) {
				out := map[int]string{0: "ORCHID-4471", 1: "CANYON-9026"}[i]
				return &domain.Handoff{FromAgent: "assistant", Payload: &domain.Payload{Data: []byte(out)}}, nil
			}
			final, err := ex.Execute(t.Context(), mergePlan(), nil, agent)
			if err != nil {
				t.Fatal(err)
			}
			if got == nil {
				t.Fatal("thought step never ran")
			}
			deps := decodeManifest(t, got)
			if len(deps) != 2 || deps[0].Step != 0 || deps[1].Step != 1 {
				t.Fatalf("manifest = %+v", deps)
			}
			for i, want := range []string{"ORCHID-4471", "CANYON-9026"} {
				d := deps[i]
				if d.Status != domain.StepDependencyCompleted || d.Content != want || d.Shortened ||
					d.Objective != mergePlan().Steps[i].Query || d.CID == "" || d.Bytes != len(want) {
					t.Errorf("dep %d = %+v", i, d)
				}
			}
			// The manifest is the step's input, never merged back into the plan context.
			for k := range final {
				if strings.Contains(k, domain.HandoffKeyStepDependencies) {
					t.Errorf("manifest leaked into the plan context as %q", k)
				}
			}
		})
	}
}

// A root step carries no manifest, even when the initial context smuggles one in.
func TestRootStep_CarriesNoManifest(t *testing.T) {
	var got *domain.Handoff
	ex := &DAGExecutor{}
	plan := &domain.ExecutionPlan{Steps: []domain.Step{{Query: "root"}}}
	_, err := ex.Execute(t.Context(), plan, map[string]string{domain.HandoffKeyStepDependencies: `[{"step":9}]`},
		func(_ context.Context, _ int, h *domain.Handoff) (*domain.Handoff, error) {
			got = h
			return &domain.Handoff{Payload: &domain.Payload{Data: []byte("x")}}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Context[domain.HandoffKeyStepDependencies]; ok {
		t.Fatalf("root step got a manifest: %v", got.Context)
	}
}

// An upstream step that finished with no output is listed as empty, not omitted.
func TestEmptyUpstream_ListedAsEmpty(t *testing.T) {
	var mu sync.Mutex
	var got *domain.Handoff
	plan := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "produce nothing"}, {Query: "produce blank"}, {Query: "use both", DependsOn: []int{0, 1}},
	}}
	_, err := (&DAGExecutor{}).Execute(t.Context(), plan, nil,
		func(_ context.Context, i int, h *domain.Handoff) (*domain.Handoff, error) {
			switch i {
			case 0:
				return &domain.Handoff{}, nil // no payload at all
			case 1:
				return &domain.Handoff{Payload: &domain.Payload{Data: []byte("  \n")}}, nil
			}
			mu.Lock()
			got = h
			mu.Unlock()
			return &domain.Handoff{Payload: &domain.Payload{Data: []byte("done")}}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	deps := decodeManifest(t, got)
	if len(deps) != 2 || deps[0].Status != domain.StepDependencyEmpty || deps[1].Status != domain.StepDependencyEmpty {
		t.Fatalf("manifest = %+v", deps)
	}
}

// A replanned plan reuses step indices: a step that now returns nothing must not
// let the previous plan's step_N_result (or its cid) stand in for its output.
func TestMergeStepResult_EmptyClearsStaleResult(t *testing.T) {
	master := map[string]string{"step_0_result": "old plan output", "step_0_cid": "old-cid"}
	mergeStepResult(bg, master, stepResult{index: 0, resp: &domain.Handoff{}}, nil, 500)
	if v, ok := master["step_0_result"]; !ok || v != "" {
		t.Errorf("step_0_result = %q (present %v), want recorded empty", v, ok)
	}
	if _, ok := master["step_0_cid"]; ok {
		t.Error("a stale cid survived an empty merge")
	}
	master = map[string]string{"step_1_cid": "old-cid"}
	mergeStepResult(bg, master, fakeResult(1, []byte("new"), nil), nil, 500)
	if _, ok := master["step_1_cid"]; ok {
		t.Error("a stale cid survived a store-less merge of a new result")
	}
}

// A resumed run's earlier step with no recorded result is listed as unavailable.
func TestBuildStepDependencies_UnavailableAndBounded(t *testing.T) {
	plan := &domain.ExecutionPlan{Steps: []domain.Step{
		{Query: "big"}, {Query: "lost"}, {Query: "use", DependsOn: []int{0, 1}},
	}}
	master := map[string]string{"step_0_result": strings.Repeat("x", 5000), "step_0_cid": "cid0"}
	sd := buildStepDependencies(master, plan, 2, domain.DependencyFit{BudgetBytes: 1000}, nil)
	deps, err := domain.DecodeStepDependencies(sd.manifest)
	if err != nil {
		t.Fatal(err)
	}
	if deps[1].Status != domain.StepDependencyUnavailable {
		t.Errorf("dep 1 = %+v", deps[1])
	}
	if !deps[0].Shortened || len(deps[0].Content)+len(deps[0].Tail) != 1000 || deps[0].Tail == "" || deps[0].Bytes != 5000 || deps[0].CID != "cid0" {
		t.Errorf("dep 0 not bounded to beginning + end + cid: shortened=%v head=%d tail=%d bytes=%d cid=%q",
			deps[0].Shortened, len(deps[0].Content), len(deps[0].Tail), deps[0].Bytes, deps[0].CID)
	}
	// The cache inputs are the FULL results, not the bounded preview.
	if len(sd.results["step_0_result"]) < 5000 {
		t.Error("cache inputs must hold the full upstream result")
	}
}

// The step cache must key a dependent step on its upstream results in Phase 3,
// where the snapshot carries no step_N_result keys.
func TestStepCache_DependentKeyedOnUpstreamResults(t *testing.T) {
	keysFor := func(upstream string) string {
		cache := &mockStepCache{}
		ex := &DAGExecutor{UseGlobalWorkspace: true, WorkspaceStage: &mockWorkspaceStage{}, StepCache: cache, CurrentSessionID: "ses-test"}
		plan := &domain.ExecutionPlan{Subject: "s", Steps: []domain.Step{
			{Query: "root", RequiredEffects: []string{}}, {Query: "dependent", DependsOn: []int{0}, RequiredEffects: []string{}},
		}}
		_, err := ex.Execute(t.Context(), plan, nil,
			func(_ context.Context, i int, _ *domain.Handoff) (*domain.Handoff, error) {
				out := "dependent-out"
				if i == 0 {
					out = upstream
				}
				return &domain.Handoff{Payload: &domain.Payload{Data: []byte(out)}}, nil
			})
		if err != nil {
			t.Fatal(err)
		}
		return cache.lastPutKey
	}
	if a, b := keysFor("upstream A"), keysFor("upstream B"); a == "" || a == b {
		t.Fatalf("dependent step keyed the same for different upstream results (%q)", a)
	}
}

// T-20261112-summarise-a-document-step-gets-only-the-first: with the budget sized
// from the receiving worker (execution.content defaults: half of a 32000-token
// prompt budget), p12's 37,444-byte report reaches the summarising step whole.
func TestBuildStepDependencies_DeliversWholeUnderReceiverBudget(t *testing.T) {
	report := strings.Repeat("r", 37444-len("Leipzig")) + "Leipzig"
	plan := &domain.ExecutionPlan{Steps: []domain.Step{{Query: "extract"}, {Query: "summarise", DependsOn: []int{0}}}}
	master := map[string]string{"step_0_result": report, "step_0_cid": "led_report"}
	fit := domain.DependencyFit{BudgetBytes: 16000 * 4, PageBytes: 16000} // config.ContentConfig{} defaults
	deps, err := domain.DecodeStepDependencies(buildStepDependencies(master, plan, 1, fit, nil).manifest)
	if err != nil {
		t.Fatal(err)
	}
	if deps[0].Shortened || len(deps[0].Content) != 37444 || !strings.HasSuffix(deps[0].Content, "Leipzig") {
		t.Fatalf("the report was shortened: shortened=%v len=%d", deps[0].Shortened, len(deps[0].Content))
	}
}
