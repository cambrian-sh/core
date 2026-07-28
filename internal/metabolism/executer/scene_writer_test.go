package executer

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// captureSceneWriter records WriteScene calls for assertion.
type captureSceneWriter struct {
	calls     []domain.StepResult
	returnID  string
	returnErr error
	edgeCalls []specifyEdgeCall
}

type specifyEdgeCall struct {
	sourceID string
	targetID string
}

func (c *captureSceneWriter) WriteScene(_ context.Context, result domain.StepResult) (string, error) {
	c.calls = append(c.calls, result)
	return c.returnID, c.returnErr
}

func twoStepPlan() *domain.ExecutionPlan {
	return &domain.ExecutionPlan{
		Steps: []domain.Step{
			{Query: "step 0"},
			{Query: "step 1", DependsOn: []int{0}},
		},
	}
}

func okStepFn(_ context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
	return &domain.Handoff{Payload: &domain.Payload{Data: []byte("result")}}, nil
}

// Cycle 1: nil SceneWriter → step completes without panic (tracer bullet).
func TestSceneWriter_NilIsNoop(t *testing.T) {
	ex := &DAGExecutor{SceneWriter: nil}
	plan := &domain.ExecutionPlan{Steps: []domain.Step{{Query: "step 0"}}}
	if _, err := ex.Execute(context.Background(), plan, nil, StepFunc(okStepFn)); err != nil {
		t.Fatalf("Execute with nil SceneWriter: %v", err)
	}
}

// captureRecorder records WritePlanScene calls (ADR-0049 D5).
//
// It keeps the WHOLE record, not just the goal: the PlanRecord is assembled
// field-by-field at the call site, so a field that stops being copied there is
// invisible to any assertion that only reads one other field.
type captureRecorder struct {
	planGoals []string
	recs      []domain.PlanRecord
}

func (c *captureRecorder) RecordExecution(_ context.Context, _ domain.StepResult) error { return nil }
func (c *captureRecorder) WritePlanScene(_ context.Context, rec domain.PlanRecord) error {
	c.planGoals = append(c.planGoals, rec.Goal)
	c.recs = append(c.recs, rec)
	return nil
}

// ADR-0049 D5: scenes are no longer per-step — the per-step WriteScene is not called.
func TestScenes_NoLongerWrittenPerStep(t *testing.T) {
	sw := &captureSceneWriter{returnID: "scene-id-1"}
	ex := &DAGExecutor{SceneWriter: sw}

	if _, err := ex.Execute(context.Background(), twoStepPlan(), nil, StepFunc(okStepFn)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(sw.calls) != 0 {
		t.Errorf("per-step WriteScene must not be called (scenes are plan-wide); got %d", len(sw.calls))
	}
}

// ADR-0049 D5: exactly one plan scene is written at completion, carrying the goal.
func TestWritePlanScene_OncePerPlan(t *testing.T) {
	rec := &captureRecorder{}
	ex := &DAGExecutor{MemoryRecorder: rec}

	plan := twoStepPlan()
	plan.Subject = "the plan goal"
	if _, err := ex.Execute(context.Background(), plan, nil, StepFunc(okStepFn)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(rec.planGoals) != 1 || rec.planGoals[0] != "the plan goal" {
		t.Errorf("expected exactly one plan scene with the goal; got %v", rec.planGoals)
	}
}

// ADR-0094 D8: the routines that informed the plan must reach the PlanRecord, or the
// memory agent's co-evolution branch (`len(rec.FollowedProcedures) > 0`) is
// unreachable and no routine's confidence ever moves from an outcome.
//
// This asserts the EXECUTOR hop specifically. The end-to-end feedback test in
// internal/memory builds its PlanRecord directly, so it passes with this hop deleted —
// which it was, for as long as the field existed.
func TestWritePlanScene_CarriesFollowedProcedures(t *testing.T) {
	rec := &captureRecorder{}
	ex := &DAGExecutor{MemoryRecorder: rec}

	plan := twoStepPlan()
	plan.FollowedProcedures = []string{"routine-a", "routine-b"}
	if _, err := ex.Execute(context.Background(), plan, nil, StepFunc(okStepFn)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(rec.recs) != 1 {
		t.Fatalf("expected one plan record; got %d", len(rec.recs))
	}
	got := rec.recs[0].FollowedProcedures
	if len(got) != 2 || got[0] != "routine-a" || got[1] != "routine-b" {
		t.Errorf("PlanRecord dropped the followed routines: got %v, want [routine-a routine-b]", got)
	}
}
