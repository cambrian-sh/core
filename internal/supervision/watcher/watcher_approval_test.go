package watcher

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// recordingExecutor is a domain.PlanExecutor that reports each plan it is asked
// to run on a channel.
type recordingExecutor struct {
	ran chan *domain.ExecutionPlan
}

func (e *recordingExecutor) Execute(_ context.Context, plan *domain.ExecutionPlan, _ *domain.Handoff) (*domain.Handoff, error) {
	e.ran <- plan
	return &domain.Handoff{}, nil
}

func onePlan(calls *int32) *testPlanner {
	return &testPlanner{planFn: func(_ context.Context, _ string) (*domain.ExecutionPlan, error) {
		atomic.AddInt32(calls, 1)
		return &domain.ExecutionPlan{Subject: "restock", Steps: []domain.Step{{Query: "order 10 GPUs"}}}, nil
	}}
}

// OnSignal plans ONCE and queues the plan on the operator approval hub; an
// approval runs it through the plan executor (ADR-0009, ticket
// T-20261031-oss-signalstream-plans-each-signal-twice-and).
func TestWatcher_OnSignal_ApprovedPlanExecutes(t *testing.T) {
	t.Parallel()
	var calls int32
	hub := domain.NewInMemoryApprovalController(5 * time.Second)
	reqs, cancel := hub.Watch() // an operator is watching
	defer cancel()
	exec := &recordingExecutor{ran: make(chan *domain.ExecutionPlan, 1)}
	w := New(nil, nil, onePlan(&calls), hub, exec, WatcherConfig{})

	if err := w.OnSignal(t.Context(), domain.Signal{StreamID: "STOCK_LOW", FromAgent: "stock_daemon", RawText: "GPU:5"}); err != nil {
		t.Fatalf("OnSignal: %v", err)
	}

	var req domain.ApprovalRequest
	select {
	case req = <-reqs:
	case <-time.After(2 * time.Second):
		t.Fatal("plan was not queued for operator approval")
	}
	if req.ToolName != ProactivePlanApprovalKind || req.AgentID != "stock_daemon" {
		t.Errorf("approval request = %+v, want kind %q from stock_daemon", req, ProactivePlanApprovalKind)
	}
	if !strings.Contains(req.ArgsPreview, "order 10 GPUs") {
		t.Errorf("preview must show the plan's steps, got %q", req.ArgsPreview)
	}
	select {
	case <-exec.ran:
		t.Fatal("plan executed before an operator decided")
	default:
	}

	if !hub.Submit(req.ID, true, "op-1") {
		t.Fatal("Submit: unknown approval id")
	}
	select {
	case p := <-exec.ran:
		if len(p.Steps) != 1 || p.Steps[0].Query != "order 10 GPUs" {
			t.Errorf("executed plan = %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("approved plan was not executed")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("planner calls = %d, want 1", got)
	}
}

func TestWatcher_OnSignal_RejectedPlanDoesNotExecute(t *testing.T) {
	t.Parallel()
	var calls int32
	hub := domain.NewInMemoryApprovalController(5 * time.Second)
	reqs, cancel := hub.Watch()
	defer cancel()
	exec := &recordingExecutor{ran: make(chan *domain.ExecutionPlan, 1)}
	w := New(nil, nil, onePlan(&calls), hub, exec, WatcherConfig{})

	_ = w.OnSignal(t.Context(), domain.Signal{StreamID: "STOCK_LOW", FromAgent: "d"})
	req := <-reqs
	hub.Submit(req.ID, false, "op-1")

	select {
	case <-exec.ran:
		t.Fatal("rejected plan executed")
	case <-time.After(200 * time.Millisecond):
	}
}

// No operator watching: the hub fails closed and the plan never runs.
func TestWatcher_OnSignal_NoApproverFailsClosed(t *testing.T) {
	t.Parallel()
	var calls int32
	hub := domain.NewInMemoryApprovalController(5 * time.Second)
	exec := &recordingExecutor{ran: make(chan *domain.ExecutionPlan, 1)}
	w := New(nil, nil, onePlan(&calls), hub, exec, WatcherConfig{})

	_ = w.OnSignal(t.Context(), domain.Signal{StreamID: "STOCK_LOW", FromAgent: "d"})
	select {
	case <-exec.ran:
		t.Fatal("plan executed with no operator to approve it")
	case <-time.After(200 * time.Millisecond):
	}
}

// A Watcher built without an approval path plans but never executes.
func TestWatcher_OnSignal_UnwiredApprovalNeverExecutes(t *testing.T) {
	t.Parallel()
	var calls int32
	w := New(nil, nil, onePlan(&calls), nil, nil, WatcherConfig{})
	if err := w.OnSignal(t.Context(), domain.Signal{StreamID: "S"}); err != nil {
		t.Fatalf("OnSignal: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("planner calls = %d, want 1", got)
	}
}

// Plans awaiting a decision are bounded; the overflow is dropped, not queued.
func TestWatcher_OnSignal_PendingPlansBounded(t *testing.T) {
	t.Parallel()
	var calls int32
	hub := domain.NewInMemoryApprovalController(5 * time.Second)
	reqs, cancel := hub.Watch()
	defer cancel()
	exec := &recordingExecutor{ran: make(chan *domain.ExecutionPlan, 4)}
	w := New(nil, nil, onePlan(&calls), hub, exec, WatcherConfig{MaxPendingPlans: 1})

	_ = w.OnSignal(t.Context(), domain.Signal{StreamID: "A"})
	first := <-reqs
	_ = w.OnSignal(t.Context(), domain.Signal{StreamID: "B"}) // bound reached: dropped

	select {
	case r := <-reqs:
		t.Fatalf("second plan was queued past the bound: %+v", r)
	case <-time.After(200 * time.Millisecond):
	}
	hub.Submit(first.ID, false, "op")
}
