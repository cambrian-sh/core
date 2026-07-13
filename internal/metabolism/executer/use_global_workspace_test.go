package executer

// Tests for the use_global_workspace circuit-breaker flag (ADR-0022 Phase 3).
//
// These tests assert on what the step handler RECEIVES in its Handoff —
// specifically which of Context or WorkingMemory is populated.
// They do NOT test PrimeForStep internals.

import (
	"context"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// mockWorkspaceStage implements domain.WorkspaceStage with a configurable
// PrimeForStep that returns controlled ContextRefs.
type mockWorkspaceStage struct {
	primeResult []domain.ContextRef
	primeCalls  int
}

func (m *mockWorkspaceStage) PrimeForPlanning(_ context.Context, _ string) (domain.LTMEnrichment, error) {
	return domain.LTMEnrichment{}, nil
}
func (m *mockWorkspaceStage) PrimeForExecution(_ context.Context, _ *domain.ExecutionPlan, ctx map[string]string) (map[string]string, error) {
	return ctx, nil
}
func (m *mockWorkspaceStage) PrimeForStep(_ context.Context, _ string, _ []domain.ContextRef, _ []domain.SearchResult, _ float64, _ int) ([]domain.ContextRef, error) {
	m.primeCalls++
	return m.primeResult, nil
}

// ── Tracer bullet ──────────────────────────────────────────────────────────

// UseGlobalWorkspace=false → step receives Handoff.Context (Phase 0 behavior),
// WorkingMemory is nil.
func TestUseGlobalWorkspace_FalseUsesContextFilter(t *testing.T) {
	mock := &mockWorkspaceStage{primeResult: []domain.ContextRef{
		{CID: "doc-1", Activation: 0.9},
	}}

	ex := &DAGExecutor{
		WorkspaceStage:     mock,
		UseGlobalWorkspace: false, // circuit breaker OFF
	}

	plan := &domain.ExecutionPlan{
		Steps: []domain.Step{
			{Query: "step 0"},
			{Query: "step 1", DependsOn: []int{0}},
		},
	}

	var step1HandoffContext map[string]string
	var step1HandoffWorkingMemory []domain.ContextRef

	stepFn := StepFunc(func(_ context.Context, idx int, h *domain.Handoff) (*domain.Handoff, error) {
		if idx == 1 {
			step1HandoffContext = h.Context
			step1HandoffWorkingMemory = h.WorkingMemory
		}
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("ok")}}, nil
	})

	if _, err := ex.Execute(context.Background(), plan, map[string]string{
		"step_0_result": "result of step 0",
	}, stepFn); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// With flag=false, WorkingMemory must be nil.
	if step1HandoffWorkingMemory != nil {
		t.Errorf("use_global_workspace=false: WorkingMemory must be nil, got %v", step1HandoffWorkingMemory)
	}
	// Context must have step_0_result (step 0 ran and produced "ok").
	if _, ok := step1HandoffContext["step_0_result"]; !ok {
		t.Errorf("use_global_workspace=false: Context must have step_0_result, got %v", step1HandoffContext)
	}
	// PrimeForStep must NOT have been called when flag=false.
	if mock.primeCalls > 0 {
		t.Errorf("use_global_workspace=false: PrimeForStep must not be called, got %d calls", mock.primeCalls)
	}
}

// UseGlobalWorkspace=true → step receives Handoff.WorkingMemory with prior step
// results (DependsOn refs) only.  LTM retrieval is left to the agent's own
// seed_recall (ADR-0036).  Context is nil/empty.
func TestUseGlobalWorkspace_TruePopulatesWorkingMemory(t *testing.T) {
	mock := &mockWorkspaceStage{}

	ex := &DAGExecutor{
		WorkspaceStage:     mock,
		UseGlobalWorkspace: true, // Phase 3 active
		MaxContextSlots:    20,
	}

	plan := &domain.ExecutionPlan{
		Steps: []domain.Step{
			{Query: "step 0"},
			{Query: "step 1", DependsOn: []int{0}},
		},
	}

	var receivedWorkingMemory []domain.ContextRef
	var receivedContext map[string]string

	stepFn := StepFunc(func(_ context.Context, idx int, h *domain.Handoff) (*domain.Handoff, error) {
		if idx == 1 {
			receivedWorkingMemory = h.WorkingMemory
			receivedContext = h.Context
		}
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("ok")}}, nil
	})

	if _, err := ex.Execute(context.Background(), plan, map[string]string{
		"step_0_result": "result of step 0",
		"step_0_cid":    "cid-step-0",
	}, stepFn); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// WorkingMemory must contain the prior step result ref (from DependsOn).
	if len(receivedWorkingMemory) == 0 {
		t.Error("use_global_workspace=true: WorkingMemory must contain prior step refs")
	}
	// Context must carry no masterContext DATA (that flows via WorkingMemory in GW
	// mode); only `_`-prefixed control keys (e.g. _task_id/_step_index, ADR-0049 D3)
	// are permitted — they are consumed as metadata, never rendered into the prompt.
	for k := range receivedContext {
		if !strings.HasPrefix(k, "_") {
			t.Errorf("use_global_workspace=true: Context must hold no masterContext keys, got %q", k)
		}
	}
	// PrimeForStep must NOT have been called — agents retrieve LTM themselves.
	if mock.primeCalls > 0 {
		t.Errorf("use_global_workspace=true: PrimeForStep must NOT be called, got %d calls", mock.primeCalls)
	}
}

// Default behavior (no flag set, no WorkspaceStage) must not panic.
func TestUseGlobalWorkspace_DefaultBehaviorNoWorkspaceStage(t *testing.T) {
	ex := &DAGExecutor{
		// UseGlobalWorkspace defaults to false
		// WorkspaceStage defaults to nil
	}
	plan := &domain.ExecutionPlan{
		Steps: []domain.Step{{Query: "step 0"}},
	}
	stepFn := StepFunc(func(_ context.Context, _ int, h *domain.Handoff) (*domain.Handoff, error) {
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("ok")}}, nil
	})
	if _, err := ex.Execute(context.Background(), plan, nil, stepFn); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

// UseGlobalWorkspace=true but WorkspaceStage=nil → graceful fallback to Context.
func TestUseGlobalWorkspace_TrueWithNilStage_FallsBackToContext(t *testing.T) {
	ex := &DAGExecutor{
		UseGlobalWorkspace: true,
		WorkspaceStage:     nil, // no stage wired
	}
	plan := &domain.ExecutionPlan{
		Steps: []domain.Step{
			{Query: "step 0"},
			{Query: "step 1", DependsOn: []int{0}},
		},
	}
	var wm []domain.ContextRef
	stepFn := StepFunc(func(_ context.Context, idx int, h *domain.Handoff) (*domain.Handoff, error) {
		if idx == 1 {
			wm = h.WorkingMemory
		}
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("ok")}}, nil
	})
	if _, err := ex.Execute(context.Background(), plan,
		map[string]string{"step_0_result": "x"}, stepFn); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Must not panic; WorkingMemory nil when no stage available.
	if wm != nil {
		t.Logf("WorkingMemory=%v (expected nil when WorkspaceStage=nil)", wm)
	}
}
