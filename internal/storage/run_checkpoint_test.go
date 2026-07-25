package storage

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

func newRunTestAdapter(t *testing.T) *BBoltAdapter {
	t.Helper()
	a, err := NewBBoltAdapterNoScan(filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatalf("open bbolt: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// The ordering bug this phase exists to kill: bbolt cursors are lexicographic, so an
// unpadded decimal step index sorted "10" before "2". "The latest checkpoint" was then
// whichever step happened to sort last, not the furthest the run actually got.
func TestListCheckpoints_OrdersNumericallyNotLexicographically(t *testing.T) {
	a := newRunTestAdapter(t)
	run := domain.RunID("run-1")

	// Save out of order, and across the 9→10 boundary that breaks string sorting.
	for _, step := range []int{2, 10, 0, 9, 1} {
		if err := a.SaveCheckpoint(run, step, map[string]string{"at": "x"}); err != nil {
			t.Fatalf("save step %d: %v", step, err)
		}
	}

	metas, err := a.ListCheckpoints(run)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := make([]int, len(metas))
	for i, m := range metas {
		got[i] = m.StepIndex
	}
	want := []int{0, 1, 2, 9, 10}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("checkpoints out of order: got %v, want %v", got, want)
		}
	}
	// The last entry must be the highest step — this is what "resume from the latest"
	// depends on being true.
	if metas[len(metas)-1].StepIndex != 10 {
		t.Errorf("latest checkpoint = step %d, want 10", metas[len(metas)-1].StepIndex)
	}
}

// Checkpoints are keyed by RUN, so one run's progress never leaks into another's — the old
// key listed every plan in the whole session.
func TestListCheckpoints_ScopedToRun(t *testing.T) {
	a := newRunTestAdapter(t)
	if err := a.SaveCheckpoint("run-A", 5, map[string]string{"k": "a"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := a.SaveCheckpoint("run-B", 1, map[string]string{"k": "b"}); err != nil {
		t.Fatalf("save: %v", err)
	}

	metas, err := a.ListCheckpoints("run-B")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(metas) != 1 || metas[0].StepIndex != 1 {
		t.Fatalf("run-B should see only its own checkpoint, got %+v", metas)
	}
}

func TestSaveLoadCheckpoint_RoundTrips(t *testing.T) {
	a := newRunTestAdapter(t)
	want := map[string]string{"step_0": "done", "note": "keep"}
	if err := a.SaveCheckpoint("run-1", 3, want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := a.LoadCheckpoint("run-1", 3)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != len(want) || got["step_0"] != "done" || got["note"] != "keep" {
		t.Errorf("round-trip mismatch: got %v want %v", got, want)
	}
}

func TestLoadCheckpoint_UnknownIsEmptyNotError(t *testing.T) {
	a := newRunTestAdapter(t)
	got, err := a.LoadCheckpoint("nope", 0)
	if err != nil {
		t.Fatalf("unknown checkpoint should not error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil context, got %v", got)
	}
}

// A run persists its PLAN. Without it a step index has no steps to index into, which is
// what made resume unsound.
func TestSaveRun_PersistsThePlan(t *testing.T) {
	a := newRunTestAdapter(t)
	run := domain.Run{
		ID:        "run-1",
		SessionID: "sess-1",
		Subject:   "ship it",
		Status:    domain.RunRunning,
		StartedAt: time.Now(),
		Plan: &domain.ExecutionPlan{
			Subject: "ship it",
			Steps:   []domain.Step{{Query: "a"}, {Query: "b", DependsOn: []int{0}}},
		},
	}
	if err := a.SaveRun(run); err != nil {
		t.Fatalf("save run: %v", err)
	}

	got, err := a.GetRun("run-1")
	if err != nil || got == nil {
		t.Fatalf("get run: %v (nil=%t)", err, got == nil)
	}
	if got.Plan == nil || len(got.Plan.Steps) != 2 {
		t.Fatalf("the plan must survive persistence, got %+v", got.Plan)
	}
	if got.Plan.Steps[1].DependsOn[0] != 0 {
		t.Error("plan dependencies must round-trip")
	}
	if !got.Resumable() {
		t.Error("a running run with a plan must be resumable")
	}
}

func TestGetRun_UnknownIsNilNotError(t *testing.T) {
	a := newRunTestAdapter(t)
	got, err := a.GetRun("nope")
	if err != nil {
		t.Fatalf("unknown run should not error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil run, got %+v", got)
	}
}

func TestListRunsForSession_FiltersAndOrders(t *testing.T) {
	a := newRunTestAdapter(t)
	base := time.Now()
	for i, r := range []domain.Run{
		{ID: "r2", SessionID: "s1", StartedAt: base.Add(2 * time.Minute)},
		{ID: "r1", SessionID: "s1", StartedAt: base},
		{ID: "rX", SessionID: "s2", StartedAt: base.Add(time.Minute)},
	} {
		if err := a.SaveRun(r); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}

	got, err := a.ListRunsForSession("s1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 runs for s1, got %d", len(got))
	}
	if got[0].ID != "r1" || got[1].ID != "r2" {
		t.Errorf("runs must be oldest-first, got %s then %s", got[0].ID, got[1].ID)
	}
}

// A finished run is not resumable — resuming it would re-run sealed work.
func TestRun_ResumableRules(t *testing.T) {
	plan := &domain.ExecutionPlan{Steps: []domain.Step{{Query: "a"}}}
	cases := []struct {
		name string
		run  domain.Run
		want bool
	}{
		{"running with plan", domain.Run{Status: domain.RunRunning, Plan: plan}, true},
		{"completed", domain.Run{Status: domain.RunCompleted, Plan: plan}, false},
		{"failed", domain.Run{Status: domain.RunFailed, Plan: plan}, false},
		{"no plan", domain.Run{Status: domain.RunRunning}, false},
		{"empty plan", domain.Run{Status: domain.RunRunning, Plan: &domain.ExecutionPlan{}}, false},
	}
	for _, c := range cases {
		if got := c.run.Resumable(); got != c.want {
			t.Errorf("%s: Resumable() = %t, want %t", c.name, got, c.want)
		}
	}
}
