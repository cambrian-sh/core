package executer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// mockStepCache is a controllable domain.StepCache for DAGExecutor integration tests.
type mockStepCache struct {
	getResult *domain.Handoff
	getOk     bool
	getErr    error
	putErr    error

	getCalls int
	putCalls int
	lastPutKey string
	lastPutTTL time.Duration
}

func (m *mockStepCache) Get(_ context.Context, _ string) (*domain.Handoff, bool, error) {
	m.getCalls++
	return m.getResult, m.getOk, m.getErr
}

func (m *mockStepCache) Put(_ context.Context, key string, _ *domain.Handoff, ttl time.Duration) error {
	m.putCalls++
	m.lastPutKey = key
	m.lastPutTTL = ttl
	return m.putErr
}

// Cycle 1 — cache hit: stepFn is NOT called; cached Handoff reaches the result.
func TestDAGExecutor_CacheHit_SkipsStepFn(t *testing.T) {
	mc := &mockStepCache{
		getResult: &domain.Handoff{
			FromAgent: "cached-agent",
			Payload:   &domain.Payload{Data: []byte("cached result")},
		},
		getOk: true,
	}

	ex := &DAGExecutor{StepCache: mc}
	plan := &domain.ExecutionPlan{
		Subject: "test plan",
		Steps:   []domain.Step{{Query: "step 0"}},
	}

	stepFnCalled := false
	stepFn := func(_ context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
		stepFnCalled = true
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("live result")}}, nil
	}

	result, err := ex.Execute(t.Context(), plan, nil, stepFn)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if stepFnCalled {
		t.Error("stepFn must NOT be called on cache hit")
	}
	if result["step_0_result"] != "cached result" {
		t.Errorf("expected cached result in masterContext, got %q", result["step_0_result"])
	}
	if mc.putCalls != 0 {
		t.Errorf("Put must NOT be called on cache hit, got %d calls", mc.putCalls)
	}
}

// Cycle 2 — cache miss: stepFn IS called and result is Put to cache.
func TestDAGExecutor_CacheMiss_CallsStepFnAndPuts(t *testing.T) {
	mc := &mockStepCache{getOk: false}

	ex := &DAGExecutor{StepCache: mc}
	plan := &domain.ExecutionPlan{
		Subject: "test plan",
		Steps:   []domain.Step{{Query: "step 0"}},
	}

	stepFnCalled := false
	stepFn := func(_ context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
		stepFnCalled = true
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("live result")}}, nil
	}

	result, err := ex.Execute(t.Context(), plan, nil, stepFn)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !stepFnCalled {
		t.Error("stepFn must be called on cache miss")
	}
	if mc.getCalls != 1 {
		t.Errorf("expected 1 Get call, got %d", mc.getCalls)
	}
	if mc.putCalls != 1 {
		t.Errorf("expected 1 Put call after successful step, got %d", mc.putCalls)
	}
	if result["step_0_result"] != "live result" {
		t.Errorf("expected live result in masterContext, got %q", result["step_0_result"])
	}
}

// Cycle 3 — Get error falls through: stepFn is still called, error not surfaced.
func TestDAGExecutor_CacheGetError_FallsThrough(t *testing.T) {
	mc := &mockStepCache{getErr: errors.New("bbolt transient error")}

	ex := &DAGExecutor{StepCache: mc}
	plan := &domain.ExecutionPlan{
		Subject: "test plan",
		Steps:   []domain.Step{{Query: "step 0"}},
	}

	stepFnCalled := false
	stepFn := func(_ context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
		stepFnCalled = true
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("fallback result")}}, nil
	}

	result, err := ex.Execute(t.Context(), plan, nil, stepFn)
	if err != nil {
		t.Fatalf("Execute must not fail when cache returns error: %v", err)
	}
	if !stepFnCalled {
		t.Error("stepFn must be called when cache Get returns error")
	}
	if result["step_0_result"] != "fallback result" {
		t.Errorf("expected fallback result, got %q", result["step_0_result"])
	}
}

// Cycle 4 — nil StepCache is backward-compatible (no change in behaviour).
func TestDAGExecutor_NilCache_BackwardCompatible(t *testing.T) {
	ex := &DAGExecutor{StepCache: nil}
	plan := &domain.ExecutionPlan{
		Subject: "test plan",
		Steps:   []domain.Step{{Query: "step 0"}},
	}

	result, err := ex.Execute(t.Context(), plan, nil, okStep("plain result", nil))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result["step_0_result"] != "plain result" {
		t.Errorf("expected plain result, got %q", result["step_0_result"])
	}
}

// Cycle 5 — on a cache hit the live stepFn is NOT invoked.
func TestDAGExecutor_CacheHit_SkipsLiveStepFn(t *testing.T) {
	mc := &mockStepCache{
		getResult: &domain.Handoff{Payload: &domain.Payload{Data: []byte("cached")}},
		getOk:     true,
	}

	liveCalled := false
	liveStep := func(_ context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
		liveCalled = true
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("live")}}, nil
	}

	ex := &DAGExecutor{StepCache: mc}
	plan := &domain.ExecutionPlan{
		Subject: "test plan",
		Steps:   []domain.Step{{Query: "step 0"}},
	}

	if _, err := ex.Execute(t.Context(), plan, nil, liveStep); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if liveCalled {
		t.Fatal("expected live stepFn to be skipped on cache hit")
	}
}
