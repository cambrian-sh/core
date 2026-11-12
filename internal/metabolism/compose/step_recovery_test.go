package compose

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// T-20261112-yield-sub-goal-on-a-composed-lease-runs-under (owner 2026-11-12): a
// yielded sub-goal whose need the step's composition cannot meet is not
// dispatched to that composition; it becomes the capability_missing / re-plan
// path. A sub-goal the composition covers, or one with no hint (a decomposition),
// is dispatched as before.

func composedStep(t *testing.T, c *Composer, lease string) {
	t.Helper()
	w := domain.ScoredCandidate{Agent: profileAgent("researcher")}
	task := &domain.DispatchTask{ID: "task-1", Description: "read the supplier pages", RequiredCapabilities: []string{"research"}}
	if _, _, ok, err := c.Compose(context.Background(), task, w, []domain.ScoredCandidate{w}, stepHandoff(lease)); !ok || err != nil {
		t.Fatalf("step not composed: ok=%v err=%v", ok, err)
	}
}

func subgoal(hint string, res *domain.CapabilityResolution) *domain.DispatchTask {
	t := &domain.DispatchTask{ID: "task-1/yield-1", Description: "write the CSV with PowerShell", Subgoal: true,
		CapabilityResolution: res}
	if hint != "" {
		t.RequiredCapabilities = []string{hint}
	}
	return t
}

func TestCompose_SubgoalWithUnmatchedHint_IsCapabilityMissing(t *testing.T) {
	c, _ := fixture(t)
	composedStep(t, c, "lease-s")
	// The gatekeeper could not resolve the hint to any declared capability and
	// fell back to the generalist tier.
	res := &domain.CapabilityResolution{Tier: domain.TierGeneralist, Resolved: []string{"general_purpose"},
		Unmatched: []string{"PowerShell command execution"}}
	generalist := domain.ScoredCandidate{Agent: profileAgent("assistant")}
	_, _, ok, err := c.Compose(context.Background(), subgoal("PowerShell command execution", res), generalist,
		[]domain.ScoredCandidate{generalist}, stepHandoff("lease-s"))
	var cm *domain.CapabilityMissingError
	if ok || !errors.As(err, &cm) {
		t.Fatalf("ok=%v err=%v: the sub-goal must not run under the same composition", ok, err)
	}
	if !reflect.DeepEqual(cm.Needed, []string{"PowerShell command execution"}) || cm.Source != domain.CapabilitySourceSubgoal ||
		cm.Composition == "" || domain.ClassOfStepError(err) != domain.StepFailureNeedsReplan {
		t.Fatalf("error %+v", cm)
	}
}

func TestCompose_SubgoalWithACapabilityOutsideTheComposition_IsCapabilityMissing(t *testing.T) {
	c, _ := fixture(t)
	composedStep(t, c, "lease-o") // professions: researcher (research)
	res := &domain.CapabilityResolution{Tier: domain.TierExact, Resolved: []string{"file_write"}}
	assistant := domain.ScoredCandidate{Agent: profileAgent("assistant")}
	_, _, _, err := c.Compose(context.Background(), subgoal("file_write", res), assistant,
		[]domain.ScoredCandidate{assistant}, stepHandoff("lease-o"))
	var cm *domain.CapabilityMissingError
	if !errors.As(err, &cm) || !reflect.DeepEqual(cm.Needed, []string{"file_write"}) {
		t.Fatalf("err = %v, want capability missing file_write", err)
	}
}

func TestCompose_SubgoalTheCompositionCovers_RunsUnderIt(t *testing.T) {
	c, _ := fixture(t)
	composedStep(t, c, "lease-c")
	res := &domain.CapabilityResolution{Tier: domain.TierExact, Resolved: []string{"research"}}
	w := domain.ScoredCandidate{Agent: profileAgent("researcher")}
	rt, comp, ok, err := c.Compose(context.Background(), subgoal("research", res), w, nil, stepHandoff("lease-c"))
	if err != nil || !ok || rt.Agent.ID != c.RuntimeID || !reflect.DeepEqual(comp.Professions, []string{"researcher"}) {
		t.Fatalf("covered sub-goal: ok=%v err=%v comp=%+v", ok, err, comp)
	}
}

func TestCompose_SubgoalWithoutHint_IsADecomposition(t *testing.T) {
	c, _ := fixture(t)
	composedStep(t, c, "lease-d")
	w := domain.ScoredCandidate{Agent: profileAgent("researcher")}
	if _, _, ok, err := c.Compose(context.Background(), subgoal("", nil), w, nil, stepHandoff("lease-d")); err != nil || !ok {
		t.Fatalf("a hint-less sub-goal is dispatched as before: ok=%v err=%v", ok, err)
	}
}

func TestCompose_HealRetryIsNotCheckedAsASubgoal(t *testing.T) {
	// A retry of the step itself carries the step's own requirements; it runs
	// under the lease's composition whatever the resolution says.
	c, _ := fixture(t)
	composedStep(t, c, "lease-r")
	task := &domain.DispatchTask{ID: "task-1", Description: "read the supplier pages", RequiredCapabilities: []string{"zzz"},
		CapabilityResolution: &domain.CapabilityResolution{Unmatched: []string{"zzz"}}}
	w := domain.ScoredCandidate{Agent: profileAgent("researcher")}
	if _, _, ok, err := c.Compose(context.Background(), task, w, nil, stepHandoff("lease-r")); err != nil || !ok {
		t.Fatalf("retry: ok=%v err=%v", ok, err)
	}
}

type fakeFleet []domain.AgentDefinition

func (f fakeFleet) GetAllAgents(context.Context) ([]domain.AgentDefinition, error) { return f, nil }

func TestOutsideStep_ReportsComposableToolsNotInTheComposition(t *testing.T) {
	c, _ := fixture(t)
	c.Fleet = fakeFleet{profileAgent("researcher"), profileAgent("assistant"), profileAgent("analyst"),
		{ID: "code_agent"}} // a code agent has no profile: its tools are no profession's
	comp := domain.WorkerComposition{Runtime: "composed_worker", Tools: []string{"mcp:web/search", "mcp:fs/read_file"}}
	var got []string
	for _, tl := range c.OutsideStep(context.Background(), comp) {
		got = append(got, tl.Name)
	}
	// write_file and sandbox/run are in some profession's allowlist; read_secret is
	// outside the person's scope; read_with_path has resource arguments (never
	// composable); mail/send is in no allowlist.
	want := []string{"mcp:fs/write_file", "mcp:sandbox/run"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("outside step = %v, want %v", got, want)
	}
}
