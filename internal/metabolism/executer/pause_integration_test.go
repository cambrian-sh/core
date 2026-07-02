package executer_test

import (
	"context"
	"testing"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/metabolism/executer"
)

func TestDAGExecutor_PauseController_AbortStopsExecution(t *testing.T) {
	pc := executer.NewPauseController()
	dag := &executer.DAGExecutor{PauseController: pc}

	plan := &domain.ExecutionPlan{
		Steps: []domain.Step{
			{Query: "rm -rf /tmp/cache", DependsOn: []int{}},
		},
	}

	stepCalled := false
	stepFn := func(ctx context.Context, idx int, h *domain.Handoff) (*domain.Handoff, error) {
		stepCalled = true
		return &domain.Handoff{}, nil
	}

	// Abort immediately in a goroutine.
	go func() {
		time.Sleep(10 * time.Millisecond)
		pc.Abort()
	}()

	_, err := dag.Execute(context.Background(), plan, nil, stepFn)
	if err == nil {
		t.Error("expected error after abort, got nil")
	}
	if stepCalled {
		t.Error("expected stepFn NOT to be called after abort")
	}
}

func TestDAGExecutor_PauseController_ResumeAllowsExecution(t *testing.T) {
	pc := executer.NewPauseController()
	dag := &executer.DAGExecutor{PauseController: pc}

	plan := &domain.ExecutionPlan{
		Steps: []domain.Step{
			{Query: "rm -rf /tmp/cache", DependsOn: []int{}},
		},
	}

	stepCalled := false
	stepFn := func(ctx context.Context, idx int, h *domain.Handoff) (*domain.Handoff, error) {
		stepCalled = true
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("done")}}, nil
	}

	// Resume after a short delay.
	go func() {
		time.Sleep(10 * time.Millisecond)
		pc.Resume()
	}()

	_, err := dag.Execute(context.Background(), plan, nil, stepFn)
	if err != nil {
		t.Errorf("expected no error after resume, got: %v", err)
	}
	if !stepCalled {
		t.Error("expected stepFn to be called after resume")
	}
}

func TestDAGExecutor_NoPauseController_SafeStepRunsNormally(t *testing.T) {
	dag := &executer.DAGExecutor{} // no PauseController

	plan := &domain.ExecutionPlan{
		Steps: []domain.Step{
			{Query: "fetch config", DependsOn: []int{}},
		},
	}

	called := false
	stepFn := func(ctx context.Context, idx int, h *domain.Handoff) (*domain.Handoff, error) {
		called = true
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("ok")}}, nil
	}

	_, err := dag.Execute(context.Background(), plan, nil, stepFn)
	if err != nil || !called {
		t.Errorf("expected clean execution, err=%v called=%v", err, called)
	}
}
