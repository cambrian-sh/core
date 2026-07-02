package executer

import (
	"context"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// captureSceneWriter records WriteScene calls for assertion.
type captureSceneWriter struct {
	calls      []domain.StepResult
	returnID   string
	returnErr  error
	edgeCalls  []specifyEdgeCall
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
type captureRecorder struct{ planGoals []string }

func (c *captureRecorder) RecordExecution(_ context.Context, _ domain.StepResult) error { return nil }
func (c *captureRecorder) WritePlanScene(_ context.Context, _ string, goal string, _ bool) error {
	c.planGoals = append(c.planGoals, goal)
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
