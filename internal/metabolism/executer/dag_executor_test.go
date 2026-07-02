package executer

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// step is a helper that builds a domain.Step with a dependency list.
func step(deps ...int) domain.Step {
	return domain.Step{Query: "q", DependsOn: deps}
}

// posOf returns the position of idx in order, or -1 if absent.
func posOf(order []int, idx int) int {
	for i, v := range order {
		if v == idx {
			return i
		}
	}
	return -1
}

// precedes asserts that `before` appears earlier than `after` in order.
func precedes(t *testing.T, order []int, before, after int) {
	t.Helper()
	if posOf(order, before) >= posOf(order, after) {
		t.Errorf("expected step %d before step %d in order %v", before, after, order)
	}
}

// --- Tracer bullet ---

func TestTopologicalSort_EmptyPlan(t *testing.T) {
	order, err := TopologicalSort(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(order) != 0 {
		t.Errorf("expected empty order, got %v", order)
	}
}

// --- Single step ---

func TestTopologicalSort_SingleStep(t *testing.T) {
	order, err := TopologicalSort([]domain.Step{step()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(order) != 1 || order[0] != 0 {
		t.Errorf("expected [0], got %v", order)
	}
}

// --- Linear chain ---

func TestTopologicalSort_LinearChain(t *testing.T) {
	// step0 → step1 → step2
	steps := []domain.Step{step(), step(0), step(1)}
	order, err := TopologicalSort(steps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(order) != 3 {
		t.Fatalf("expected 3 steps in order, got %v", order)
	}
	precedes(t, order, 0, 1)
	precedes(t, order, 1, 2)
}

// --- Diamond DAG ---

func TestTopologicalSort_DiamondDAG(t *testing.T) {
	// step0 and step1 are independent; step2 depends on both
	steps := []domain.Step{step(), step(), step(0, 1)}
	order, err := TopologicalSort(steps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(order) != 3 {
		t.Fatalf("expected 3 steps in order, got %v", order)
	}
	precedes(t, order, 0, 2)
	precedes(t, order, 1, 2)
}

// --- Cycles ---

func TestTopologicalSort_DirectCycle(t *testing.T) {
	// step0 → step1 → step0
	steps := []domain.Step{step(1), step(0)}
	_, err := TopologicalSort(steps)
	if err == nil {
		t.Fatal("expected CyclicPlanError, got nil")
	}
	var cycleErr *CyclicPlanError
	if !errors.As(err, &cycleErr) {
		t.Fatalf("expected *CyclicPlanError, got %T: %v", err, err)
	}
	if cycleErr.Description == "" {
		t.Error("CyclicPlanError.Description must not be empty")
	}
	// Description must name the involved steps so the Planner retry prompt is useful.
	for _, idx := range []string{"0", "1"} {
		if !containsStr(cycleErr.Description, idx) {
			t.Errorf("expected cycle description to mention step %s, got: %q", idx, cycleErr.Description)
		}
	}
}

func TestTopologicalSort_ThreeNodeCycle(t *testing.T) {
	// step0 → step1 → step2 → step0
	steps := []domain.Step{step(2), step(0), step(1)}
	_, err := TopologicalSort(steps)
	if err == nil {
		t.Fatal("expected CyclicPlanError, got nil")
	}
	var cycleErr *CyclicPlanError
	if !errors.As(err, &cycleErr) {
		t.Fatalf("expected *CyclicPlanError, got %T: %v", err, err)
	}
	if cycleErr.Description == "" {
		t.Error("CyclicPlanError.Description must not be empty")
	}
}

func TestTopologicalSort_SelfReference(t *testing.T) {
	steps := []domain.Step{step(), step(1)}
	_, err := TopologicalSort(steps)
	if err == nil {
		t.Fatal("expected error for self-referencing step, got nil")
	}
	var cycleErr *CyclicPlanError
	if !errors.As(err, &cycleErr) {
		t.Fatalf("expected *CyclicPlanError, got %T: %v", err, err)
	}
}

// --- Validation errors ---

func TestTopologicalSort_OutOfBoundsIndex(t *testing.T) {
	steps := []domain.Step{step(99)}
	_, err := TopologicalSort(steps)
	if err == nil {
		t.Fatal("expected error for out-of-bounds index, got nil")
	}
	// Must NOT be a CyclicPlanError — it is a structural/validation error.
	var cycleErr *CyclicPlanError
	if errors.As(err, &cycleErr) {
		t.Errorf("out-of-bounds should return a plain error, not *CyclicPlanError")
	}
}

// --- Duplicate edges ---

func TestTopologicalSort_DuplicateEdgesDoNotInflateInDegree(t *testing.T) {
	// step1 lists step0 twice; in-degree of step1 must still be 1, not 2.
	steps := []domain.Step{step(), step(0, 0)}
	order, err := TopologicalSort(steps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(order) != 2 {
		t.Fatalf("expected 2 steps in order, got %v", order)
	}
	precedes(t, order, 0, 1)
}

// --- helpers ---

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}

// okStep returns a StepFunc that immediately returns a Handoff with the given
// payload and optional extra context keys.
func okStep(payload string, extraCtx map[string]string) StepFunc {
	return func(_ context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
		h := &domain.Handoff{Payload: &domain.Payload{Data: []byte(payload)}}
		if len(extraCtx) > 0 {
			h.Context = extraCtx
		}
		return h, nil
	}
}

// failStep returns a StepFunc that immediately returns an error.
func failStep(msg string) StepFunc {
	return func(_ context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
		return nil, fmt.Errorf("%s", msg)
	}
}

// dispatchingStep returns a StepFunc that routes to one of two inner StepFuncs
// by step index — useful for multi-step plans with per-step behaviour.
func dispatchingStep(fns map[int]StepFunc) StepFunc {
	return func(ctx context.Context, i int, h *domain.Handoff) (*domain.Handoff, error) {
		fn, ok := fns[i]
		if !ok {
			return &domain.Handoff{Payload: &domain.Payload{Data: []byte("default")}}, nil
		}
		return fn(ctx, i, h)
	}
}

// ============================================================
// DAGExecutor tests
// ============================================================

func TestDAGExecutor_EmptyPlan(t *testing.T) {
	plan := &domain.ExecutionPlan{}
	initial := map[string]string{"seed": "value"}
	got, err := (&DAGExecutor{}).Execute(t.Context(), plan, initial, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["seed"] != "value" {
		t.Errorf("initial context not preserved: %v", got)
	}
}

func TestDAGExecutor_SingleStep_ResultInContext(t *testing.T) {
	plan := &domain.ExecutionPlan{Steps: []domain.Step{step()}}
	got, err := (&DAGExecutor{}).Execute(t.Context(), plan, nil, okStep("hello", nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["step_0_result"] != "hello" {
		t.Errorf("expected step_0_result=hello, got %v", got)
	}
	if got[finalResultKey] != "hello" {
		t.Errorf("expected finalResultKey=hello, got %q", got[finalResultKey])
	}
}

func TestDAGExecutor_IndependentStepsRunConcurrently(t *testing.T) {
	// Two independent steps. Each blocks until the other has started.
	// If they don't run concurrently, this deadlocks and times out.
	started := make(chan int, 2)
	barrier := make(chan struct{})

	stepFn := func(ctx context.Context, i int, _ *domain.Handoff) (*domain.Handoff, error) {
		started <- i
		select {
		case <-barrier:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("ok")}}, nil
	}

	// Close barrier once both steps have signalled they're running.
	go func() {
		seen := 0
		for range started {
			seen++
			if seen == 2 {
				close(barrier)
				return
			}
		}
	}()

	plan := &domain.ExecutionPlan{Steps: []domain.Step{step(), step()}}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	if _, err := (&DAGExecutor{}).Execute(ctx, plan, nil, stepFn); err != nil {
		t.Fatalf("expected concurrent execution to succeed, got: %v", err)
	}
}

func TestDAGExecutor_DependentStepSeesPredessorResult(t *testing.T) {
	// step 0 produces "from_zero"; step 1 depends on step 0 and must find
	// "step_0_result" in its context snapshot.
	var capturedCtx map[string]string

	stepFn := dispatchingStep(map[int]StepFunc{
		0: okStep("from_zero", nil),
		1: func(_ context.Context, _ int, h *domain.Handoff) (*domain.Handoff, error) {
			capturedCtx = h.Context
			return &domain.Handoff{Payload: &domain.Payload{Data: []byte("from_one")}}, nil
		},
	})

	plan := &domain.ExecutionPlan{
		Steps: []domain.Step{step(), step(0)},
	}

	if _, err := (&DAGExecutor{}).Execute(t.Context(), plan, nil, stepFn); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedCtx["step_0_result"] != "from_zero" {
		t.Errorf("step 1 context snapshot missing step_0_result, got: %v", capturedCtx)
	}
}

func TestDAGExecutor_ContextSnapshotIsolation(t *testing.T) {
	// Steps 0 and 1 are independent. They run concurrently.
	// Verify that a key written by step 0 into masterContext (via merge after
	// step 0 completes) is NOT visible in step 1's snapshot if step 1 was
	// dispatched before step 0 completed.
	//
	// We enforce ordering by making step 1 start only after step 0 has already
	// been dispatched, using a latch. Because both steps are root nodes the
	// executor dispatches them in the same dispatch() call, so both snapshots
	// are taken before either has merged its result.

	var step1Snapshot map[string]string
	var once sync.Once
	step0Started := make(chan struct{})

	stepFn := dispatchingStep(map[int]StepFunc{
		0: func(ctx context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
			close(step0Started) // signal that step 0 is running
			time.Sleep(20 * time.Millisecond)
			return &domain.Handoff{Payload: &domain.Payload{Data: []byte("step0_output")}}, nil
		},
		1: func(_ context.Context, _ int, h *domain.Handoff) (*domain.Handoff, error) {
			<-step0Started // wait until step 0 has started (but not finished)
			once.Do(func() { step1Snapshot = h.Context })
			return &domain.Handoff{Payload: &domain.Payload{Data: []byte("step1_output")}}, nil
		},
	})

	plan := &domain.ExecutionPlan{Steps: []domain.Step{step(), step()}}
	if _, err := (&DAGExecutor{}).Execute(t.Context(), plan, nil, stepFn); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, found := step1Snapshot["step_0_result"]; found {
		t.Error("step 1 snapshot should not contain step_0_result (snapshot is taken before step 0 merges)")
	}
}

func TestDAGExecutor_ResultMergeNamespacing(t *testing.T) {
	// Step 0 returns payload "pval" and Context{"foo": "bar"}.
	// Verify master context contains "step_0_result" and "step_0_foo".
	stepFn := okStep("pval", map[string]string{"foo": "bar"})
	plan := &domain.ExecutionPlan{Steps: []domain.Step{step()}}

	got, err := (&DAGExecutor{}).Execute(t.Context(), plan, nil, stepFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["step_0_result"] != "pval" {
		t.Errorf("expected step_0_result=pval, got %q", got["step_0_result"])
	}
	if got["step_0_foo"] != "bar" {
		t.Errorf("expected step_0_foo=bar, got %q", got["step_0_foo"])
	}
	if _, leaked := got["foo"]; leaked {
		t.Error("unnamespaced key 'foo' must not appear in master context")
	}
}

func TestDAGExecutor_CancelOnFirstError(t *testing.T) {
	// Step 0 fails. Step 1 is independent and in-flight; it must receive a
	// cancelled context and return. Execute must return the original error.
	step1Cancelled := make(chan struct{})

	stepFn := dispatchingStep(map[int]StepFunc{
		0: failStep("step 0 error"),
		1: func(ctx context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
			<-ctx.Done()
			close(step1Cancelled)
			return nil, ctx.Err()
		},
	})

	plan := &domain.ExecutionPlan{Steps: []domain.Step{step(), step()}}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	_, err := (&DAGExecutor{}).Execute(ctx, plan, nil, stepFn)
	if err == nil {
		t.Fatal("expected error from failing step, got nil")
	}
	if !containsStr(err.Error(), "step 0 error") {
		t.Errorf("expected original error message, got: %v", err)
	}

	select {
	case <-step1Cancelled:
		// step 1 detected cancellation
	case <-time.After(500 * time.Millisecond):
		t.Error("step 1 did not receive cancellation signal within 500ms")
	}
}

func TestDAGExecutor_WaitGroupDrainsOnCancel(t *testing.T) {
	// After Execute returns an error, all goroutines it launched must have exited.
	var running atomic.Int32
	allExited := make(chan struct{})
	var once sync.Once

	stepFn := dispatchingStep(map[int]StepFunc{
		0: failStep("step 0 error"),
		1: func(ctx context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
			running.Add(1)
			defer func() {
				if running.Add(-1) == 0 {
					once.Do(func() { close(allExited) })
				}
			}()
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})

	plan := &domain.ExecutionPlan{Steps: []domain.Step{step(), step()}}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	_, err := (&DAGExecutor{}).Execute(ctx, plan, nil, stepFn)
	if err == nil {
		t.Fatal("expected error")
	}

	// Execute already called wg.Wait() before returning, so all goroutines
	// must have exited by the time we reach here. The channel close is just
	// an explicit in-test witness.
	select {
	case <-allExited:
		// all goroutines exited before Execute returned
	default:
		// running goroutines were still counted when Execute returned
		// give them a tiny grace period for defer sequencing
		select {
		case <-allExited:
		case <-time.After(100 * time.Millisecond):
			t.Errorf("goroutine leak: %d goroutine(s) still running after Execute returned", running.Load())
		}
	}
}

func TestDAGExecutor_FanIn(t *testing.T) {
	// Steps 0 and 1 are independent. Step 2 depends on both.
	// Verify step 2's context snapshot contains both predecessor results,
	// and that the results are namespaced correctly.
	var step2Snapshot map[string]string

	stepFn := dispatchingStep(map[int]StepFunc{
		0: okStep("result_A", nil),
		1: okStep("result_B", nil),
		2: func(_ context.Context, _ int, h *domain.Handoff) (*domain.Handoff, error) {
			step2Snapshot = h.Context
			return &domain.Handoff{Payload: &domain.Payload{Data: []byte("result_C")}}, nil
		},
	})

	plan := &domain.ExecutionPlan{
		Steps: []domain.Step{step(), step(), step(0, 1)},
	}

	got, err := (&DAGExecutor{}).Execute(t.Context(), plan, nil, stepFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if step2Snapshot["step_0_result"] != "result_A" {
		t.Errorf("fan-in: step 2 missing step_0_result, snapshot: %v", step2Snapshot)
	}
	if step2Snapshot["step_1_result"] != "result_B" {
		t.Errorf("fan-in: step 2 missing step_1_result, snapshot: %v", step2Snapshot)
	}
	if got[finalResultKey] != "result_C" {
		t.Errorf("expected finalResultKey=result_C, got %q", got[finalResultKey])
	}
}

func TestDAGExecutor_NoGoroutineLeakOnSuccess(t *testing.T) {
	before := runtime.NumGoroutine()
	plan := &domain.ExecutionPlan{Steps: []domain.Step{step(), step(), step(0, 1)}}
	if _, err := (&DAGExecutor{}).Execute(t.Context(), plan, nil, okStep("ok", nil)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Allow the Go scheduler a moment to reclaim any deferred cleanup.
	time.Sleep(20 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after > before+2 {
		t.Errorf("possible goroutine leak: before=%d after=%d", before, after)
	}
}

// TestDAGExecutor_FanIn_ParallelSpeedup verifies the TIMING guarantee of the
// fan-in topology: two independent steps must run concurrently so the total
// wall-clock time is ≈ max(step0, step1) + step2, not step0+step1+step2.
//
// Topology:
//
//	Step 0 (no deps): sleeps 100ms
//	Step 1 (no deps): sleeps 150ms
//	Step 2 (deps: [0,1]): sleeps 80ms
//
// Sequential worst-case: 100+150+80 = 330ms
// Parallel ideal:        max(100,150)+80 = 230ms
// Assertion:             elapsed < 280ms (230 + 50ms) proves parallelism.
//
// Upper bound 380ms (230 + 150ms generous slack) guards against a spuriously
// slow CI runner from producing a false failure.
func TestDAGExecutor_FanIn_ParallelSpeedup(t *testing.T) {
	var step2Snapshot map[string]string

	stepFn := dispatchingStep(map[int]StepFunc{
		0: func(ctx context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
			select {
			case <-time.After(100 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &domain.Handoff{Payload: &domain.Payload{Data: []byte("result_A")}}, nil
		},
		1: func(ctx context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
			select {
			case <-time.After(150 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &domain.Handoff{Payload: &domain.Payload{Data: []byte("result_B")}}, nil
		},
		2: func(ctx context.Context, _ int, h *domain.Handoff) (*domain.Handoff, error) {
			step2Snapshot = h.Context
			select {
			case <-time.After(80 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &domain.Handoff{Payload: &domain.Payload{Data: []byte("result_C")}}, nil
		},
	})

	plan := &domain.ExecutionPlan{
		Steps: []domain.Step{step(), step(), step(0, 1)},
	}

	start := time.Now()
	got, err := (&DAGExecutor{}).Execute(t.Context(), plan, nil, stepFn)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// --- Timing assertion ---
	// < 280ms proves steps 0 and 1 ran in parallel (sequential = 330ms).
	// > 380ms catches a genuine hang or pathological scheduler stall.
	if elapsed >= 280*time.Millisecond {
		t.Errorf("parallel speedup not achieved: elapsed=%v (want < 280ms, sequential would be ~330ms)", elapsed)
	}
	if elapsed > 380*time.Millisecond {
		t.Errorf("execution took unexpectedly long: elapsed=%v (upper guard = 380ms)", elapsed)
	}
	t.Logf("TestDAGExecutor_FanIn_ParallelSpeedup: elapsed=%v", elapsed)

	// --- Correctness assertions (same guarantees as TestDAGExecutor_FanIn) ---
	if step2Snapshot["step_0_result"] != "result_A" {
		t.Errorf("fan-in speedup: step 2 missing step_0_result, snapshot: %v", step2Snapshot)
	}
	if step2Snapshot["step_1_result"] != "result_B" {
		t.Errorf("fan-in speedup: step 2 missing step_1_result, snapshot: %v", step2Snapshot)
	}
	if got[finalResultKey] != "result_C" {
		t.Errorf("expected finalResultKey=result_C, got %q", got[finalResultKey])
	}
}

// TestDAGExecutor_NoGoroutineLeakOnStepFailure verifies that when a step fails
// while a sibling goroutine is in-flight, all goroutines drain before Execute
// returns — no goroutine leak regardless of failure timing.
//
// Design:
//   - Step 0 fails immediately.
//   - Step 1 is independent (no deps) and sleeps until its context is cancelled.
//   - runtime.NumGoroutine() is sampled before and after Execute.
//   - A 50ms sleep after Execute gives the Go scheduler time to fully reclaim
//     goroutine stack pages before the final count is taken.
func TestDAGExecutor_NoGoroutineLeakOnStepFailure(t *testing.T) {
	// Ensure any goroutines from previous tests have fully exited before sampling.
	time.Sleep(20 * time.Millisecond)
	before := runtime.NumGoroutine()

	stepFn := dispatchingStep(map[int]StepFunc{
		0: failStep("step 0 failed"),
		1: func(ctx context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
			// Block until cancellation propagates from step 0's failure.
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})

	plan := &domain.ExecutionPlan{Steps: []domain.Step{step(), step()}}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	_, err := (&DAGExecutor{}).Execute(ctx, plan, nil, stepFn)
	if err == nil {
		t.Fatal("expected error from failing step, got nil")
	}
	if !containsStr(err.Error(), "step 0 failed") {
		t.Errorf("expected original error message, got: %v", err)
	}

	// Give the Go scheduler time to reclaim goroutine stacks after wg.Wait().
	time.Sleep(50 * time.Millisecond)
	after := runtime.NumGoroutine()

	// Allow +2 for benign Go runtime variance (e.g. finaliser goroutine).
	if after > before+2 {
		t.Errorf("goroutine leak after step failure: before=%d after=%d (leaked %d)", before, after, after-before)
	}
}

// mockEventWriter captures TaskEvents written by DAGExecutor.
// It implements TaskEventWriter inline so no stub file is needed.
type mockEventWriter struct {
	mu     sync.Mutex
	events []domain.TaskEvent
}

func (m *mockEventWriter) WriteTaskEvent(event domain.TaskEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, event)
	return nil
}

// TestDAGExecutor_ContextGrowthRecorded verifies that DAGExecutor calls
// WriteTaskEvent exactly once per completed step and that the recorded
// ContextGrowthBytes reflects the bytes the step added to resp.Context.
func TestDAGExecutor_ContextGrowthRecorded(t *testing.T) {
	mw := &mockEventWriter{}
	executor := &DAGExecutor{EventWriter: mw}

	// The step returns Context{"answer": "42"}.
	// Initial context is nil/empty → sizeBefore == 0.
	// sizeAfter  == len("answer")+len("42") == 6+2 == 8
	// growthBytes == 8
	plan := &domain.ExecutionPlan{
		Steps: []domain.Step{{Query: "q", DependsOn: nil}},
	}

	stepFn := func(_ context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
		return &domain.Handoff{
			FromAgent: "test-agent",
			Payload:   &domain.Payload{Data: []byte("result")},
			Context:   map[string]string{"answer": "42"},
		}, nil
	}

	_, err := executor.Execute(t.Context(), plan, nil, stepFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mw.mu.Lock()
	defer mw.mu.Unlock()

	if len(mw.events) != 1 {
		t.Fatalf("expected 1 TaskEvent, got %d", len(mw.events))
	}
	ev := mw.events[0]
	if ev.AgentID != "test-agent" {
		t.Errorf("expected AgentID=test-agent, got %q", ev.AgentID)
	}
	// growthBytes = len("answer")+len("42") - 0 == 8
	if ev.ContextGrowthBytes != 8 {
		t.Errorf("expected ContextGrowthBytes=8, got %d", ev.ContextGrowthBytes)
	}
	if ev.TaskID == "" {
		t.Error("TaskID must not be empty")
	}
}

func TestDAGExecutor_ThoughtStep_LoggedAsSystemThought(t *testing.T) {
	mw := &mockEventWriter{}
	executor := &DAGExecutor{EventWriter: mw}

	plan := &domain.ExecutionPlan{
		Steps: []domain.Step{{Query: "synthesize", IsThought: true}},
	}

	stepFn := func(_ context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
		return &domain.Handoff{
			Payload: &domain.Payload{Data: []byte("summary")},
		}, nil
	}

	_, err := executor.Execute(t.Context(), plan, nil, stepFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mw.mu.Lock()
	defer mw.mu.Unlock()

	if len(mw.events) != 1 {
		t.Fatalf("expected 1 TaskEvent, got %d", len(mw.events))
	}
	ev := mw.events[0]
	if ev.AgentID != "System_Thought" {
		t.Errorf("expected AgentID=System_Thought for thought step, got %q", ev.AgentID)
	}
}

func TestDAGExecutor_ThoughtStep_UsesThoughtFn(t *testing.T) {
	thoughtCalled := false
	thoughtFn := func(_ context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
		thoughtCalled = true
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("thought result")}}, nil
	}

	executor := &DAGExecutor{ThoughtFn: thoughtFn}

	plan := &domain.ExecutionPlan{
		Steps: []domain.Step{{Query: "think", IsThought: true}},
	}

	// stepFn should NOT be called for the thought step if ThoughtFn is provided.
	stepFnCalled := false
	stepFn := func(_ context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
		stepFnCalled = true
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("agent result")}}, nil
	}

	got, err := executor.Execute(t.Context(), plan, nil, stepFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !thoughtCalled {
		t.Error("expected ThoughtFn to be called for thought step")
	}
	if stepFnCalled {
		t.Error("expected stepFn NOT to be called for thought step")
	}
	if got["step_0_result"] != "thought result" {
		t.Errorf("expected thought result, got %q", got["step_0_result"])
	}
}

// ============================================================
// Pause/Resume/HotSwap tests
// ============================================================

func TestDAGExecutor_Pause(t *testing.T) {
	// Step 0 is independent. Step 1 depends on step 0.
	// Step 0 blocks until signalled. After step 0 is dispatched but before
	// it completes, the executor is paused. After step 0 completes, dispatch()
	// is called but step 1 must NOT be dispatched while paused.
	// Resume is called, then step 1 is dispatched and completes.

	step0Unblocked := make(chan struct{})
	step0Started := make(chan struct{})
	step1Started := make(chan struct{})

	stepFn := dispatchingStep(map[int]StepFunc{
		0: func(_ context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
			close(step0Started)
			select {
			case <-step0Unblocked:
			case <-time.After(2 * time.Second):
				return nil, fmt.Errorf("step 0 timeout")
			}
			return &domain.Handoff{Payload: &domain.Payload{Data: []byte("result_0")}}, nil
		},
		1: func(_ context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
			close(step1Started)
			return &domain.Handoff{Payload: &domain.Payload{Data: []byte("result_1")}}, nil
		},
	})

	plan := &domain.ExecutionPlan{
		Steps: []domain.Step{step(), step(0)},
	}

	executor := &DAGExecutor{MaxReplanAttempts: 2}
	execCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	go func() {
		executor.Execute(execCtx, plan, nil, stepFn)
	}()

	// Wait for step 0 to start
	select {
	case <-step0Started:
	case <-time.After(time.Second):
		t.Fatal("step 0 did not start within 1s")
	}

	// Pause the executor
	executor.Pause()

	// Unblock step 0
	close(step0Unblocked)

	// Step 1 must NOT start within a reasonable time while paused
	select {
	case <-step1Started:
		t.Error("step 1 was dispatched while paused — should NOT happen")
	case <-time.After(300 * time.Millisecond):
		// Expected: step 1 was not dispatched
	}

	// Resume
	executor.Resume()

	// Step 1 should now start
	select {
	case <-step1Started:
		// Step 1 started after resume — correct
	case <-time.After(2 * time.Second):
		t.Fatal("step 1 did not start after resume within 2s")
	}

	cancel()
}

func TestDAGExecutor_HotSwap(t *testing.T) {
	// Plan A: step 0 blocks until signalled. Pause while step 0 is in-flight.
	// HotSwap Plan B (2 independent steps). Resume. Plan B steps execute.
	step0Started := make(chan struct{})
	step0Unblocked := make(chan struct{})

	executor := &DAGExecutor{MaxReplanAttempts: 2}

	planA := &domain.ExecutionPlan{
		Steps: []domain.Step{step()},
	}

	// Track which step indices have been called
	var calledIndices []int
	var calledMu sync.Mutex
	var step0Once sync.Once

	stepFn := func(_ context.Context, i int, _ *domain.Handoff) (*domain.Handoff, error) {
		// Plan A step 0: signal and block (only first call to index 0)
		step0Once.Do(func() {
			close(step0Started)
			select {
			case <-step0Unblocked:
			case <-time.After(2 * time.Second):
			}
		})
		calledMu.Lock()
		calledIndices = append(calledIndices, i)
		calledMu.Unlock()
		return &domain.Handoff{
			Payload: &domain.Payload{Data: []byte(fmt.Sprintf("step_%d", i))},
		}, nil
	}

	execCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	var got map[string]string
	var execErr error
	done := make(chan struct{})
	go func() {
		got, execErr = executor.Execute(execCtx, planA, nil, stepFn)
		close(done)
	}()

	// Wait for Plan A step 0 to start
	select {
	case <-step0Started:
	case <-time.After(time.Second):
		t.Fatal("step 0 did not start")
	}

	// Pause while step 0 is in-flight
	executor.Pause()

	// HotSwap Plan B (2 independent steps)
	planB := &domain.ExecutionPlan{
		Steps: []domain.Step{step(), step()},
	}
	executor.HotSwap(planB)
	executor.Resume()

	// Unblock Plan A step 0
	close(step0Unblocked)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Execute did not complete within 3s")
	}

	if execErr != nil {
		t.Fatalf("expected success after hot-swap, got: %v", execErr)
	}

	calledMu.Lock()
	n := len(calledIndices)
	calledMu.Unlock()

	// Plan A step 0 + Plan B's steps (at least 2 more calls)
	if n < 3 {
		t.Errorf("expected at least 3 step calls (1 Plan A + 2 Plan B), got %d: %v", n, calledIndices)
	}

	// Verify Plan B results are in the master context
	if got["step_0_result"] != "step_0" {
		t.Errorf("expected step_0_result=step_0, got %q", got["step_0_result"])
	}
	if got["step_1_result"] != "step_1" {
		t.Errorf("expected step_1_result=step_1, got %q", got["step_1_result"])
	}
}

func TestDAGExecutor_MaxReplanExceeded(t *testing.T) {
	// MaxReplan=0. Plan A step 0 blocks. Pause while in-flight. HotSwap.
	// Resume. Unblock step 0. On resume, replanCount(1) > MaxReplan(0) →
	// PartialPlanError.
	step0Started := make(chan struct{})
	step0Unblocked := make(chan struct{})
	var step0Once sync.Once

	executor := &DAGExecutor{MaxReplanAttempts: 0}

	planA := &domain.ExecutionPlan{
		Steps: []domain.Step{step()},
	}

	stepFn := func(_ context.Context, i int, _ *domain.Handoff) (*domain.Handoff, error) {
		step0Once.Do(func() {
			close(step0Started)
			select {
			case <-step0Unblocked:
			case <-time.After(2 * time.Second):
			}
		})
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte("result")}}, nil
	}

	execCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	var execErr error
	done := make(chan struct{})
	go func() {
		_, execErr = executor.Execute(execCtx, planA, nil, stepFn)
		close(done)
	}()

	// Wait for step 0 to start
	select {
	case <-step0Started:
	case <-time.After(time.Second):
		t.Fatal("step 0 did not start")
	}

	executor.Pause()
	planB := &domain.ExecutionPlan{Steps: []domain.Step{step()}}
	executor.HotSwap(planB)
	executor.Resume()
	close(step0Unblocked)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Execute did not complete within 2s")
	}

	if execErr == nil {
		t.Fatal("expected PartialPlanError, got nil")
	}
	var partialErr *PartialPlanError
	if !errors.As(execErr, &partialErr) {
		t.Fatalf("expected *PartialPlanError, got %T: %v", execErr, execErr)
	}
	if partialErr.ReplanCount != 1 {
		t.Errorf("expected ReplanCount=1, got %d", partialErr.ReplanCount)
	}
}

func TestDAGExecutor_PartialPlanErrorOnFailure(t *testing.T) {
	// 3 linear steps: step0 succeeds, step1 succeeds, step2 fails.
	plan := &domain.ExecutionPlan{
		Steps: []domain.Step{step(), step(0), step(1)},
	}

	stepFn := dispatchingStep(map[int]StepFunc{
		0: okStep("result_0", nil),
		1: okStep("result_1", nil),
		2: failStep("step 2 failed"),
	})

	_, err := (&DAGExecutor{}).Execute(t.Context(), plan, nil, stepFn)
	if err == nil {
		t.Fatal("expected PartialPlanError, got nil")
	}

	var partialErr *PartialPlanError
	if !errors.As(err, &partialErr) {
		t.Fatalf("expected *PartialPlanError, got %T: %v", err, err)
	}

	if partialErr.Context["step_0_result"] != "result_0" {
		t.Errorf("expected step_0_result=result_0, got %q", partialErr.Context["step_0_result"])
	}
	if partialErr.Context["step_1_result"] != "result_1" {
		t.Errorf("expected step_1_result=result_1, got %q", partialErr.Context["step_1_result"])
	}
	if _, found := partialErr.Context["step_2_result"]; found {
		t.Error("step_2_result must not be in partial context")
	}
	if partialErr.FailedStep != 2 {
		t.Errorf("expected FailedStep=2, got %d", partialErr.FailedStep)
	}
	if !containsStr(partialErr.LastError.Error(), "step 2 failed") {
		t.Errorf("expected LastError to contain 'step 2 failed', got: %v", partialErr.LastError)
	}
	if partialErr.ReplanCount != 0 {
		t.Errorf("expected ReplanCount=0, got %d", partialErr.ReplanCount)
	}
}

func TestDAGExecutor_PartialPlanError_InitialContextPreserved(t *testing.T) {
	// Step0 fails immediately; initial context should be preserved in PartialPlanError.
	plan := &domain.ExecutionPlan{
		Steps: []domain.Step{step()},
	}
	initial := map[string]string{"original_prompt": "do X"}

	_, err := (&DAGExecutor{}).Execute(t.Context(), plan, initial, failStep("step 0 error"))
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var partialErr *PartialPlanError
	if !errors.As(err, &partialErr) {
		t.Fatalf("expected *PartialPlanError, got %T: %v", err, err)
	}
	if partialErr.Context["original_prompt"] != "do X" {
		t.Errorf("initial context not preserved: %v", partialErr.Context)
	}
	if !containsStr(partialErr.Error(), "step 0 error") {
		t.Errorf("Error() should mention original error: %s", partialErr.Error())
	}
}

func TestPartialPlanError_Unwrap(t *testing.T) {
	inner := errors.New("inner error")
	partial := &PartialPlanError{FailedStep: 0, LastError: inner}

	if !errors.Is(partial, inner) {
		t.Error("errors.Is should unwrap to inner error")
	}
}

func TestDAGExecutor_CancelOnFirstError_BackwardCompatible(t *testing.T) {
	// Existing tests like CancelOnFirstError check err != nil and err.Error().
	// PartialPlanError satisfies error interface so these invariants hold.
	step1Cancelled := make(chan struct{})

	stepFn := dispatchingStep(map[int]StepFunc{
		0: failStep("step 0 error"),
		1: func(ctx context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
			<-ctx.Done()
			close(step1Cancelled)
			return nil, ctx.Err()
		},
	})

	plan := &domain.ExecutionPlan{Steps: []domain.Step{step(), step()}}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	_, err := (&DAGExecutor{}).Execute(ctx, plan, nil, stepFn)
	if err == nil {
		t.Fatal("expected error from failing step, got nil")
	}
	// Backward compatibility: err != nil check still works.
	if !containsStr(err.Error(), "step 0 error") {
		t.Errorf("expected error message to contain 'step 0 error', got: %v", err)
	}

	select {
	case <-step1Cancelled:
	case <-time.After(500 * time.Millisecond):
		t.Error("step 1 did not receive cancellation signal within 500ms")
	}
}

func TestDAGExecutor_AgentThoughtAgentFlow(t *testing.T) {
	// Step 0: Agent (finds "A")
	// Step 1: Thought (synthesizes "A" into "A synthesized")
	// Step 2: Agent (receives "A synthesized", returns "A synthesized and B")

	plan := &domain.ExecutionPlan{
		Steps: []domain.Step{
			{Query: "find A"},                         // Step 0
			{Query: "synthesize", IsThought: true, DependsOn: []int{0}}, // Step 1
			{Query: "final step", DependsOn: []int{1}}, // Step 2
		},
	}

	thoughtFn := func(_ context.Context, i int, h *domain.Handoff) (*domain.Handoff, error) {
		prev := h.Context["step_0_result"]
		return &domain.Handoff{Payload: &domain.Payload{Data: []byte(prev + " synthesized")}}, nil
	}

	stepFn := func(_ context.Context, i int, h *domain.Handoff) (*domain.Handoff, error) {
		if i == 0 {
			return &domain.Handoff{Payload: &domain.Payload{Data: []byte("A")}}, nil
		}
		if i == 2 {
			prev := h.Context["step_1_result"]
			return &domain.Handoff{Payload: &domain.Payload{Data: []byte(prev + " and B")}}, nil
		}
		return nil, fmt.Errorf("unexpected step index %d", i)
	}

	executor := &DAGExecutor{ThoughtFn: thoughtFn}
	got, err := executor.Execute(t.Context(), plan, nil, stepFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got["step_0_result"] != "A" {
		t.Errorf("step 0 failed, got %q", got["step_0_result"])
	}
	if got["step_1_result"] != "A synthesized" {
		t.Errorf("step 1 (thought) failed, got %q", got["step_1_result"])
	}
	if got["step_2_result"] != "A synthesized and B" {
		t.Errorf("step 2 failed, got %q", got["step_2_result"])
	}
	if got[finalResultKey] != "A synthesized and B" {
		t.Errorf("final result mismatch, got %q", got[finalResultKey])
	}
}

// ── H1 coordinator gate tests (issue 0013-03) ────────────────────────────────

// fakeValidator is a CheckpointValidator stub returning a canned verdict and
// counting calls (ADR-0013 H1 gate, post-validator-agent migration).
type fakeValidator struct {
	coherent   bool
	assessment string
	calls      *int
}

func (f fakeValidator) Validate(_ context.Context, _ domain.CheckpointRequest) (domain.CheckpointVerdict, error) {
	if f.calls != nil {
		*f.calls++
	}
	return domain.CheckpointVerdict{Coherent: f.coherent, Assessment: f.assessment}, nil
}

// cleanValidator always reports coherence; incoherentValidator always fails.
func cleanValidator() domain.CheckpointValidator {
	return fakeValidator{coherent: true, assessment: "VERDICT: COHERENT looks good"}
}

func incoherentValidator(reason string) domain.CheckpointValidator {
	return fakeValidator{coherent: false, assessment: "VERDICT: INCOHERENT " + reason}
}

// Cycle 1: CheckpointAfter=true with a clean ThoughtFn → Execute succeeds and
// the successor step is dispatched.
func TestDAGExecutor_CheckpointGate_CleanPath_SuccessorDispatched(t *testing.T) {
	successorRan := false
	stepFn := dispatchingStep(map[int]StepFunc{
		1: func(_ context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
			successorRan = true
			return &domain.Handoff{Payload: &domain.Payload{Data: []byte("successor result")}}, nil
		},
	})

	plan := &domain.ExecutionPlan{
		Steps: []domain.Step{
			{Query: "step 0", DependsOn: nil, CheckpointAfter: true},
			{Query: "step 1", DependsOn: []int{0}},
		},
	}
	executor := &DAGExecutor{CheckpointValidator: cleanValidator()}

	_, err := executor.Execute(t.Context(), plan, nil, stepFn)
	if err != nil {
		t.Fatalf("expected clean execution, got error: %v", err)
	}
	if !successorRan {
		t.Error("successor step (step 1) was not dispatched after clean checkpoint")
	}
}

// Cycle 2: CheckpointAfter=true, ThoughtFn emits REPLAN_SIGNAL → Execute
// returns a PartialPlanError whose LastError is a *SemanticCheckpointError.
func TestDAGExecutor_CheckpointGate_Incoherence_ReturnsSemanticCheckpointError(t *testing.T) {
	successorRan := false
	stepFn := dispatchingStep(map[int]StepFunc{
		1: func(_ context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
			successorRan = true
			return &domain.Handoff{Payload: &domain.Payload{Data: []byte("should not run")}}, nil
		},
	})

	plan := &domain.ExecutionPlan{
		Steps: []domain.Step{
			{Query: "step 0", DependsOn: nil, CheckpointAfter: true},
			{Query: "step 1", DependsOn: []int{0}},
		},
	}
	executor := &DAGExecutor{CheckpointValidator: incoherentValidator("output is wrong")}

	_, execErr := executor.Execute(t.Context(), plan, nil, stepFn)
	if execErr == nil {
		t.Fatal("expected error from incoherent checkpoint, got nil")
	}

	var partialErr *PartialPlanError
	if !errors.As(execErr, &partialErr) {
		t.Fatalf("expected *PartialPlanError, got %T: %v", execErr, execErr)
	}

	var checkpointErr *domain.SemanticCheckpointError
	if !errors.As(partialErr.LastError, &checkpointErr) {
		t.Fatalf("expected LastError to be *SemanticCheckpointError, got %T: %v", partialErr.LastError, partialErr.LastError)
	}
	if checkpointErr.StepIndex != 0 {
		t.Errorf("expected StepIndex=0, got %d", checkpointErr.StepIndex)
	}
	if successorRan {
		t.Error("successor step (step 1) must NOT run after incoherent checkpoint")
	}
}

// Cycle 3: assessment is stored in masterContext as step_{i}_checkpoint and
// accessible via PartialPlanError.Context in the incoherent case.
func TestDAGExecutor_CheckpointGate_AssessmentStoredInContext(t *testing.T) {
	plan := &domain.ExecutionPlan{
		Steps: []domain.Step{
			{Query: "step 0", DependsOn: nil, CheckpointAfter: true},
			{Query: "step 1", DependsOn: []int{0}},
		},
	}
	executor := &DAGExecutor{CheckpointValidator: incoherentValidator("bad result")}

	_, execErr := executor.Execute(t.Context(), plan, nil, okStep("step0 output", nil))
	var partialErr *PartialPlanError
	if !errors.As(execErr, &partialErr) {
		t.Fatalf("expected *PartialPlanError, got %T", execErr)
	}

	assessment, ok := partialErr.Context["step_0_checkpoint"]
	if !ok {
		t.Fatal("expected step_0_checkpoint key in PartialPlanError.Context")
	}
	if !containsStr(assessment, "INCOHERENT") {
		t.Errorf("step_0_checkpoint must contain the verdict assessment, got %q", assessment)
	}
}

// Cycle 4: CheckpointAfter=false → the validator is never invoked, Execute succeeds.
func TestDAGExecutor_CheckpointGate_Skipped_WhenCheckpointAfterFalse(t *testing.T) {
	calls := 0
	plan := &domain.ExecutionPlan{
		Steps: []domain.Step{
			{Query: "step 0", DependsOn: nil}, // CheckpointAfter defaults to false
			{Query: "step 1", DependsOn: []int{0}},
		},
	}
	executor := &DAGExecutor{CheckpointValidator: fakeValidator{coherent: true, calls: &calls}}

	_, err := executor.Execute(t.Context(), plan, nil, okStep("result", nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 0 {
		t.Errorf("validator must not be called when CheckpointAfter=false, got %d call(s)", calls)
	}
}

// Cycle 5: CheckpointAfter=true with nil CheckpointValidator → gate is skipped
// entirely, Execute succeeds and successor is dispatched.
func TestDAGExecutor_CheckpointGate_Skipped_WhenValidatorNil(t *testing.T) {
	successorRan := false
	stepFn := dispatchingStep(map[int]StepFunc{
		1: func(_ context.Context, _ int, _ *domain.Handoff) (*domain.Handoff, error) {
			successorRan = true
			return &domain.Handoff{Payload: &domain.Payload{Data: []byte("ok")}}, nil
		},
	})

	plan := &domain.ExecutionPlan{
		Steps: []domain.Step{
			{Query: "step 0", DependsOn: nil, CheckpointAfter: true},
			{Query: "step 1", DependsOn: []int{0}},
		},
	}
	executor := &DAGExecutor{CheckpointValidator: nil} // no validator wired

	_, err := executor.Execute(t.Context(), plan, nil, stepFn)
	if err != nil {
		t.Fatalf("expected success when validator is nil, got: %v", err)
	}
	if !successorRan {
		t.Error("successor must be dispatched when validator is nil (gate skipped)")
	}
}

// capturingReplanHandler captures the error passed to Replan and returns a
// trivial one-step plan so the executor can complete without looping.
type capturingReplanHandler struct {
	capturedErr error
}

func (h *capturingReplanHandler) Replan(
	_ context.Context, _ int, err error,
	_ map[string]string, _ *domain.ExecutionPlan,
) (*domain.ExecutionPlan, error) {
	h.capturedErr = err
	return &domain.ExecutionPlan{
		Steps:   []domain.Step{{Query: "repaired step", DependsOn: nil}},
		Subject: "Replan",
	}, nil
}

// Cycle 6: when gate fires and ReplanHandler is wired, Replan is called with
// a *SemanticCheckpointError as the error argument.
func TestDAGExecutor_CheckpointGate_Incoherence_CallsReplanHandler(t *testing.T) {
	handler := &capturingReplanHandler{}
	plan := &domain.ExecutionPlan{
		Steps: []domain.Step{
			{Query: "step 0", DependsOn: nil, CheckpointAfter: true},
		},
	}
	executor := &DAGExecutor{
		CheckpointValidator: incoherentValidator("bad output"),
		ReplanHandler:       handler,
		MaxReplanAttempts:   1,
	}

	executor.Execute(t.Context(), plan, nil, okStep("result", nil)) //nolint:errcheck

	if handler.capturedErr == nil {
		t.Fatal("ReplanHandler.Replan was not called")
	}
	var checkpointErr *domain.SemanticCheckpointError
	if !errors.As(handler.capturedErr, &checkpointErr) {
		t.Fatalf("expected Replan to receive *SemanticCheckpointError, got %T: %v", handler.capturedErr, handler.capturedErr)
	}
}

// mockCheckpointStore records which step indices were saved.
type mockCheckpointStore struct {
	mu      sync.Mutex
	saved   []int
}

func (m *mockCheckpointStore) SaveCheckpoint(_ string, _ string, stepIndex int, _ map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saved = append(m.saved, stepIndex)
	return nil
}

func (m *mockCheckpointStore) LoadCheckpoint(_ string, _ string, _ int) (map[string]string, error) {
	return nil, nil
}

func (m *mockCheckpointStore) ListCheckpoints(_ string) ([]CheckpointMeta, error) {
	return nil, nil
}

// Cycle 7: CheckpointStore.SaveCheckpoint is called for the step even when the
// gate subsequently detects incoherence and blocks dispatch.
func TestDAGExecutor_CheckpointGate_CheckpointSavedBeforeGateBlocks(t *testing.T) {
	cs := &mockCheckpointStore{}
	plan := &domain.ExecutionPlan{
		Steps: []domain.Step{
			{Query: "step 0", DependsOn: nil, CheckpointAfter: true},
			{Query: "step 1", DependsOn: []int{0}},
		},
	}
	executor := &DAGExecutor{
		CheckpointValidator: incoherentValidator("bad output"),
		CheckpointStore:     cs,
	}

	executor.Execute(t.Context(), plan, nil, okStep("result", nil)) //nolint:errcheck

	cs.mu.Lock()
	defer cs.mu.Unlock()
	found := false
	for _, idx := range cs.saved {
		if idx == 0 {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected CheckpointStore.SaveCheckpoint to be called for step 0 before gate blocked, saved steps: %v", cs.saved)
	}
}

// ── runCheckpoint tests (issue 0013-02) ──────────────────────────────────────

// Cycle 1: nil ThoughtFn returns ("", false) without panicking.
// capturingValidator records the CheckpointRequest it received and returns a
// canned verdict.
type capturingValidator struct {
	got      domain.CheckpointRequest
	coherent bool
}

func (c *capturingValidator) Validate(_ context.Context, req domain.CheckpointRequest) (domain.CheckpointVerdict, error) {
	c.got = req
	return domain.CheckpointVerdict{Coherent: c.coherent, Assessment: "VERDICT: COHERENT"}, nil
}

func TestRunCheckpoint_NilValidator_ReturnsFalse(t *testing.T) {
	d := &DAGExecutor{CheckpointValidator: nil}
	step := domain.Step{Query: "do something"}
	assessment, incoherent := d.runCheckpoint(t.Context(), 0, "plan-abc", step, map[string]string{})
	if assessment != "" {
		t.Errorf("expected empty assessment, got %q", assessment)
	}
	if incoherent {
		t.Error("expected incoherent=false when validator is nil")
	}
}

// Cycle 2: validator reports INCOHERENT → incoherent=true with the verdict text.
func TestRunCheckpoint_IncoherentVerdict_IncoherentTrue(t *testing.T) {
	d := &DAGExecutor{CheckpointValidator: incoherentValidator("output is wrong")}
	step := domain.Step{Query: "do something"}
	assessment, incoherent := d.runCheckpoint(t.Context(), 0, "plan-abc", step, map[string]string{})
	if !incoherent {
		t.Error("expected incoherent=true when validator reports INCOHERENT")
	}
	if !containsStr(assessment, "INCOHERENT") {
		t.Errorf("expected the verdict assessment, got %q", assessment)
	}
}

// Cycle 3: validator reports COHERENT → incoherent=false.
func TestRunCheckpoint_CoherentVerdict_IncoherentFalse(t *testing.T) {
	d := &DAGExecutor{CheckpointValidator: cleanValidator()}
	step := domain.Step{Query: "do something"}
	_, incoherent := d.runCheckpoint(t.Context(), 0, "plan-abc", step, map[string]string{})
	if incoherent {
		t.Error("expected incoherent=false on a COHERENT verdict")
	}
}

// Cycle 4: runCheckpoint passes the step query and custom checkpoint question
// through to the validator request (no prompt building in the executor).
func TestRunCheckpoint_PassesRequestToValidator(t *testing.T) {
	cv := &capturingValidator{coherent: true}
	d := &DAGExecutor{CheckpointValidator: cv}
	step := domain.Step{Query: "original intent", CheckpointQuery: "custom: is this coherent?"}
	masterCtx := map[string]string{"step_0_result": "the produced output"}
	d.runCheckpoint(t.Context(), 0, "plan-abc", step, masterCtx)

	if cv.got.StepQuery != "original intent" {
		t.Errorf("StepQuery = %q, want original intent", cv.got.StepQuery)
	}
	if cv.got.Question != "custom: is this coherent?" {
		t.Errorf("Question = %q, want the custom checkpoint query", cv.got.Question)
	}
	if cv.got.StepOutput != "the produced output" {
		t.Errorf("StepOutput = %q, want the step result from masterContext", cv.got.StepOutput)
	}
}

// Cycle 6: EventWriter.WriteTaskEvent is called with AgentID="System_Checkpoint"
// and TaskID="checkpoint-{stepIndex}-{planID}".
func TestRunCheckpoint_EventWriter_CalledWithCorrectFields(t *testing.T) {
	mw := &mockEventWriter{}
	d := &DAGExecutor{CheckpointValidator: cleanValidator(), EventWriter: mw}
	step := domain.Step{Query: "do work"}
	d.runCheckpoint(t.Context(), 3, "myplan42", step, map[string]string{})

	mw.mu.Lock()
	defer mw.mu.Unlock()

	if len(mw.events) != 1 {
		t.Fatalf("expected 1 TaskEvent from EventWriter, got %d", len(mw.events))
	}
	ev := mw.events[0]
	if ev.AgentID != "System_Checkpoint" {
		t.Errorf("expected AgentID=System_Checkpoint, got %q", ev.AgentID)
	}
	wantTaskID := "checkpoint-3-myplan42"
	if ev.TaskID != wantTaskID {
		t.Errorf("expected TaskID=%q, got %q", wantTaskID, ev.TaskID)
	}
}

// Cycle 7: EnqueueVerification is never called during runCheckpoint.
func TestRunCheckpoint_EnqueueVerification_NeverCalled(t *testing.T) {
	var enqueueCallCount int
	capturingEnqueue := EnqueueVerification(func(taskID, agentID string, req, resp *domain.Handoff) {
		enqueueCallCount++
	})
	d := &DAGExecutor{CheckpointValidator: cleanValidator(), EnqueueVerification: capturingEnqueue}
	step := domain.Step{Query: "do work"}
	d.runCheckpoint(t.Context(), 0, "plan-abc", step, map[string]string{})
	if enqueueCallCount != 0 {
		t.Errorf("expected EnqueueVerification to not be called, got %d call(s)", enqueueCallCount)
	}
}

// --- Observer telemetry ---

type testObserver struct {
	onTaskCompleted func(domain.TaskEvent)
}

func (o *testObserver) OnTaskCompleted(evt domain.TaskEvent) { o.onTaskCompleted(evt) }
func (o *testObserver) OnSessionEvicted(_ string)             {}
func (o *testObserver) OnConwipWait(_ int64)                  {}
func (o *testObserver) OnAuctionNoWinner(_ string)            {}
func (o *testObserver) OnSchemaMismatch(_, _ string)          {}
func (o *testObserver) OnPlanCompleted(_ domain.PlanEvent)                 {}
func (o *testObserver) OnRetrievalCompleted(_ domain.RetrievalSession)       {}
func (o *testObserver) OnContradictionResolved(_ domain.ContradictionResolution) {}

func TestDAGExecutor_ObserverOnTaskCompletedCalled(t *testing.T) {
	var captured domain.TaskEvent
	observer := &testObserver{onTaskCompleted: func(evt domain.TaskEvent) {
		captured = evt
	}}

	dag := &DAGExecutor{
		Observer:    observer,
		EventWriter: &mockEventWriter{},
	}

	plan := &domain.ExecutionPlan{Steps: []domain.Step{{Query: "do work"}}}
	stepFn := StepFunc(func(_ context.Context, i int, _ *domain.Handoff) (*domain.Handoff, error) {
		return &domain.Handoff{
			FromAgent: "agent-1",
			Payload:   &domain.Payload{Data: []byte("response")},
		}, nil
	})

	_, err := dag.Execute(t.Context(), plan, nil, stepFn)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if captured.AgentID != "agent-1" {
		t.Errorf("Observer.AgentID = %q, want agent-1", captured.AgentID)
	}
	if captured.TaskID == "" {
		t.Error("Observer.TaskID is empty")
	}
}
