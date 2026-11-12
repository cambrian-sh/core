package domain

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Owner decision 2026-11-12 (T-20261112-yield-sub-goal-on-a-composed-lease-runs-under):
// a step stops after 2-3 identical (tool, args, error) failures and that error goes
// to the re-planner. The count is the kernel's, per step lease.

func failingExecutor(threshold int) (*ToolExecutor, *fakeHandler) {
	reg := NewInMemoryToolRegistry()
	reg.Register(SystemTool{Name: "mcp:filesystem/write_file"})
	reg.Register(SystemTool{Name: "mcp:filesystem/read_file"})
	h := &fakeHandler{err: errors.New("Error: file was replaced while it was being opened")}
	e := &ToolExecutor{Registry: reg, Grants: NewInMemoryGrantsStore(), Handler: h, Unrestricted: true,
		InlineThreshold: 1 << 20, StepFailures: NewStepFailures(threshold)}
	return e, h
}

func writeCall(lease LeaseID, args string) ToolCallRequest {
	return ToolCallRequest{AgentID: "composed_worker", ToolName: "mcp:filesystem/write_file", ArgsJSON: []byte(args), SessionTokenID: lease}
}

func TestStepFailures_IdenticalFailureStopsTheStep(t *testing.T) {
	e, h := failingExecutor(3)
	ctx := WithStepCall(context.Background(), StepCall{RunID: "run-1", StepIndex: 0})

	r1 := e.Execute(ctx, writeCall("lease-1", `{"path":"out.csv","content":"a"}`))
	if strings.Contains(r1.Error, "[") {
		t.Fatalf("first failure carries no note: %q", r1.Error)
	}
	// The same call in another key order is the same call.
	r2 := e.Execute(ctx, writeCall("lease-1", `{"content":"a","path":"out.csv"}`))
	if !strings.Contains(r2.Error, "failed 2 times") || !strings.Contains(r2.Error, "will not succeed") {
		t.Fatalf("second identical failure must carry the hint: %q", r2.Error)
	}
	r3 := e.Execute(ctx, writeCall("lease-1", `{"path":"out.csv","content":"a"}`))
	if !strings.Contains(r3.Error, "The step is stopped") {
		t.Fatalf("third identical failure stops the step: %q", r3.Error)
	}
	stuck := e.StepFailures.Stuck("lease-1")
	if stuck == nil || stuck.Tool != "mcp:filesystem/write_file" || stuck.Count != 3 ||
		ClassOfStepError(stuck) != StepFailureNeedsReplan {
		t.Fatalf("stuck = %+v", stuck)
	}

	// Every later call on the stopped step is refused without running.
	h.called = false
	r4 := e.Execute(ctx, ToolCallRequest{AgentID: "composed_worker", ToolName: "mcp:filesystem/read_file",
		ArgsJSON: []byte(`{"path":"out.csv"}`), SessionTokenID: "lease-1"})
	if !r4.Denied || h.called || !strings.Contains(r4.DenyReason, "No further tool call runs") {
		t.Fatalf("call after the stop: denied=%v ran=%v reason=%q", r4.Denied, h.called, r4.DenyReason)
	}

	// Another step's lease is unaffected; the lease's end forgets it.
	if r := e.Execute(ctx, writeCall("lease-2", `{"path":"out.csv","content":"a"}`)); r.Denied {
		t.Fatalf("another step was stopped: %q", r.DenyReason)
	}
	e.EndRun("lease-1")
	if e.StepFailures.Stuck("lease-1") != nil {
		t.Fatal("EndRun must forget the lease")
	}
}

func TestStepFailures_DifferentArgsOrErrorsAreNotALoop(t *testing.T) {
	e, _ := failingExecutor(3)
	ctx := WithStepCall(context.Background(), StepCall{RunID: "run-1", StepIndex: 0})
	for _, p := range []string{"a.csv", "b.csv", "c.csv", "d.csv"} {
		e.Execute(ctx, writeCall("lease-1", `{"path":"`+p+`"}`))
	}
	if e.StepFailures.Stuck("lease-1") != nil {
		t.Fatal("a worker changing its arguments is working around the error, not looping")
	}
}

func TestStepFailures_OnlyPlanStepsAreTracked(t *testing.T) {
	// A chat turn's lease (no plan step behind it) is not stopped.
	e, _ := failingExecutor(2)
	for i := 0; i < 3; i++ {
		e.Execute(context.Background(), writeCall("chat-lease", `{"path":"a.csv"}`))
	}
	if e.StepFailures.Stuck("chat-lease") != nil {
		t.Fatal("a non-step lease was stopped")
	}
}

func TestStepFailures_OffAndUntracked(t *testing.T) {
	if NewStepFailures(0) != nil || NewStepFailures(1) != nil {
		t.Fatal("0 and 1 disable the stop")
	}
	var off *StepFailures
	if n, stuck := off.Observe("l", "t", nil, "e"); n != 0 || stuck || off.Stuck("l") != nil {
		t.Fatal("nil tracker must be inert")
	}
	f := NewStepFailures(2)
	if n, _ := f.Observe("", "t", nil, "e"); n != 0 {
		t.Fatal("a call with no lease is not a step and is not tracked")
	}
}
