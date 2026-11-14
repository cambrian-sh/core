package network

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/metabolism"
	"github.com/cambrian-sh/core/internal/metabolism/agentmgr"
	"github.com/cambrian-sh/core/internal/metabolism/dispatch"
	supgk "github.com/cambrian-sh/core/internal/supervision/gatekeeper"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The progress-aware plan ceiling (owner decision 2026-11-05,
// T-20261105-owner-decision-needed-120-s-plan-timeout-ms), at scaled-down
// durations: an idle window of ~150 ms stands in for the 180 s default, and the
// "old 120 s cap" is any multiple of it.

// --- unit: the watchdog itself -------------------------------------------------

func TestPlanCeiling_KeepsAPlanThatKeepsMovingAlive(t *testing.T) {
	t.Parallel()
	act := newPlanActivity()
	ctx, stop := startPlanCeiling(context.Background(), time.Minute, 60*time.Millisecond, act.at)
	defer stop()

	// Ten idle windows of real time, with a step boundary every 20 ms.
	deadline := time.Now().Add(600 * time.Millisecond)
	for time.Now().Before(deadline) {
		act.touch()
		time.Sleep(20 * time.Millisecond)
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("a plan that kept making progress was cut: %v (%v)", err, context.Cause(ctx))
	}
}

func TestPlanCeiling_InFlightWorkIsActivity(t *testing.T) {
	t.Parallel()
	act := newPlanActivity()
	ctx, stop := startPlanCeiling(context.Background(), time.Minute, 60*time.Millisecond, act.at)
	defer stop()

	done := act.begin() // one long in-kernel call: no boundary for 6 windows
	time.Sleep(360 * time.Millisecond)
	if err := ctx.Err(); err != nil {
		t.Fatalf("an in-flight call was read as idle: %v", context.Cause(ctx))
	}
	done()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the plan was not cut once the call returned and nothing followed")
	}
	if c := context.Cause(ctx); !strings.Contains(c.Error(), "execution.plan.plan_idle_timeout_ms") {
		t.Fatalf("cause = %v, want the idle cause", c)
	}
}

func TestPlanCeiling_IdleAndMaxDurationCausesAreDistinctTimeouts(t *testing.T) {
	t.Parallel()
	frozen := time.Now()
	idleCtx, stopIdle := startPlanCeiling(context.Background(), time.Minute, 50*time.Millisecond,
		func() time.Time { return frozen })
	defer stopIdle()
	<-idleCtx.Done()
	idleCause := planCeilingCause(context.Background(), idleCtx)
	if idleCause == nil || !errors.Is(idleCause, context.DeadlineExceeded) ||
		!strings.Contains(idleCause.Error(), "plan idle for") ||
		!strings.Contains(idleCause.Error(), "execution.plan.plan_idle_timeout_ms") {
		t.Fatalf("idle cause = %v", idleCause)
	}

	maxCtx, stopMax := startPlanCeiling(context.Background(), 80*time.Millisecond, time.Minute, time.Now)
	defer stopMax()
	<-maxCtx.Done()
	maxCause := planCeilingCause(context.Background(), maxCtx)
	if maxCause == nil || !errors.Is(maxCause, context.DeadlineExceeded) ||
		!strings.Contains(maxCause.Error(), "plan exceeded max duration") ||
		!strings.Contains(maxCause.Error(), "execution.plan.plan_max_duration_ms") {
		t.Fatalf("max-duration cause = %v", maxCause)
	}

	// The caller giving up is not the plan timing out.
	parent, cancel := context.WithCancel(context.Background())
	callerCtx, stopCaller := startPlanCeiling(parent, time.Minute, time.Minute, time.Now)
	defer stopCaller()
	cancel()
	<-callerCtx.Done()
	if c := planCeilingCause(parent, callerCtx); c != nil {
		t.Fatalf("caller cancellation reported as a plan timeout: %v", c)
	}
}

func TestPlanLimits_ZeroIdleDisablesAndZeroCapFallsBackToDefault(t *testing.T) {
	t.Parallel()
	maxD, idle := planLimits(config.PlanConfig{})
	if maxD != time.Duration(config.DefaultPlanMaxDurationMs)*time.Millisecond || idle != 0 {
		t.Fatalf("planLimits(zero) = %s, %s", maxD, idle)
	}
	ctx, stop := startPlanCeiling(context.Background(), time.Minute, 0, func() time.Time { return time.Time{} })
	defer stop()
	time.Sleep(30 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatal("idle=0 must disable the watchdog")
	}
}

// --- gateway: a plan's leases ------------------------------------------------------

// A long unary model call on a step lease bound to the run counts as activity for
// as long as it is in flight; a tracked tool call does too.
func TestGateway_RunActivityCountsInFlightCallsOnTheRunsLeases(t *testing.T) {
	t.Parallel()
	gw := NewLLMGateway(config.DefaultConfig().Execution)
	m := gatedModel{started: make(chan struct{}), release: make(chan struct{})}
	useFakeModel(gw, "llm:m1", m)
	lease, err := gw.Acquire(context.Background(),
		domain.StepAllocation{Winner: domain.AgentDefinition{ID: "llm:m1"}}, 4096, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	gw.BindLease(lease, domain.LeaseBinding{RunID: "run-1", StepIndex: 0})

	done := make(chan error, 1)
	go func() {
		_, err := gw.GenerateWithTools(context.Background(), lease, domain.UserPrompt("long"))
		done <- err
	}()
	<-m.started
	time.Sleep(40 * time.Millisecond)
	if got := gw.RunActivity("run-1"); time.Since(got) > 10*time.Millisecond {
		t.Fatalf("in-flight call: run activity %s ago, want ≈ now", time.Since(got))
	}
	if got := gw.RunActivity("run-2"); !got.IsZero() {
		t.Fatalf("another run must see nothing, got %s", got)
	}
	close(m.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// A tool call tracked on the same lease is in flight too.
	time.Sleep(30 * time.Millisecond)
	end := gw.TrackLeaseWork(lease)
	if got := gw.RunActivity("run-1"); time.Since(got) > 10*time.Millisecond {
		t.Fatalf("tracked tool call: run activity %s ago, want ≈ now", time.Since(got))
	}
	end()
	if st := gw.GetBudgetLeaseState(lease); st == nil || st.InFlightCalls != 0 {
		t.Fatalf("want 0 in flight after the tool call, got %+v", st)
	}
	gw.TrackLeaseWork("")() // empty lease: a no-op, never a panic
}

// --- Execute end to end ----------------------------------------------------------

// sleepyModel answers after d, or when its ctx ends.
type sleepyModel struct{ d time.Duration }

func (m sleepyModel) GenerateRequest(ctx context.Context, _ domain.GenerateRequest) (domain.GenerateResponse, error) {
	select {
	case <-time.After(m.d):
		return domain.GenerateResponse{Text: "ok", StopReason: domain.StopEndTurn}, nil
	case <-ctx.Done():
		return domain.GenerateResponse{}, ctx.Err()
	}
}

func (sleepyModel) StreamRequest(context.Context, domain.GenerateRequest) (<-chan domain.StreamChunk, error) {
	ch := make(chan domain.StreamChunk)
	close(ch)
	return ch, nil
}

func (sleepyModel) Capabilities() domain.Capabilities { return domain.Capabilities{} }

// watchdogServer is a minimal Execute fixture: one agent, a plan of `steps` steps,
// a real gateway (so step leases and their in-flight counts exist), and the
// caller standing in for the agent.
func watchdogServer(t *testing.T, steps int, plan config.PlanConfig, caller callerFunc, model any) *Server {
	t.Helper()
	reg := metabolism.NewInMemoryRegistry()
	reg.SetAgent(domain.AgentDefinition{ID: "worker", Name: "worker", Runtime: domain.RuntimePython, Trait: domain.TraitTool})
	mgr := agentmgr.NewAgentManager(reg, "python", "unix://tmp/cambrian.sock", nil)
	gk := supgk.NewGatekeeper(reg, config.ExecutionConfig{Gatekeeper: config.GatekeeperConfig{
		GatekeeperMaxCandidates: 5, GatekeeperW1: 0.4, GatekeeperW2: 0.4, GatekeeperW3: 0.2,
	}})
	gw := NewLLMGateway(config.DefaultConfig().Execution)
	if model != nil {
		useFakeModel(gw, "llm:m1", model)
		gw.SetDefaultModelID("llm:m1")
	}
	ps := make([]domain.Step, steps)
	for i := range ps {
		ps[i] = domain.Step{Query: "work"}
		if i > 0 {
			ps[i].DependsOn = []int{i - 1}
		}
	}
	return &Server{
		Planner: &mockPlanner{responses: []plannerResponse{{plan: &domain.ExecutionPlan{Subject: "s", Steps: ps}}}},
		Manager: mgr,
		Dispatcher: &dispatch.Dispatcher{
			Gatekeeper: gk, Caller: caller, Manifests: mgr, Boots: mgr,
			ExecCfg: config.ExecutionConfig{Gatekeeper: config.GatekeeperConfig{GatekeeperMaxCandidates: 5}},
		},
		LLMGateway: gw,
		ExecCfg:    config.ExecutionConfig{Plan: plan},
	}
}

func stepLease(h *domain.Handoff) domain.LeaseID {
	return domain.LeaseID(h.Context["_session_token_id"])
}

func okHandoff(agentID string) *domain.Handoff {
	return &domain.Handoff{Payload: &domain.Payload{Data: []byte("done")}, FromAgent: agentID}
}

// requireCeilingTimeout accepts both shapes Execute reports a cut plan in — a
// partial-plan handoff stamped _plan_timed_out, or a DeadlineExceeded status —
// and returns the cause text.
func requireCeilingTimeout(t *testing.T, resp *pb.Handoff, err error) string {
	t.Helper()
	if err != nil {
		st, _ := status.FromError(err)
		if st.Code() != codes.DeadlineExceeded {
			t.Fatalf("want DeadlineExceeded, got %v: %v", st.Code(), err)
		}
		return st.Message()
	}
	if resp == nil || resp.Metadata[PlanTimedOutKey] != "true" {
		t.Fatalf("want a timed-out plan, got %+v", resp)
	}
	return resp.Metadata[PlanTimeoutCauseKey] + " | " + string(resp.Payload.Data)
}

// A plan whose steps keep making progress runs well past several idle windows (the
// scaled stand-in for the old 120 s cap) and is not cut.
func TestExecute_PlanThatKeepsMakingProgressOutlivesTheIdleWindow(t *testing.T) {
	t.Parallel()
	caller := callerFunc(func(ctx context.Context, agentID string, _ *domain.Handoff, _ string) (*domain.Handoff, error) {
		// Each step: 1/3 of an idle window of work. It was 100 ms of a 150 ms window,
		// which a loaded machine (the whole tree under `go test`, Windows) stretched
		// past the window — "plan idle for 158ms, limit 150ms" (T-20261108-testexecute).
		// 200 ms of slack per step keeps the assertion about progress, not timing.
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return okHandoff(agentID), nil
	})
	s := watchdogServer(t, 10, config.PlanConfig{PlanIdleTimeoutMs: 300, PlanMaxDurationMs: 60000}, caller, nil)
	start := time.Now()
	resp, err := s.Execute(t.Context(), &pb.Handoff{Payload: &pb.Object{Data: []byte("go")}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.Metadata[PlanTimedOutKey] == "true" || resp.Metadata["_partial_plan"] == "true" {
		t.Fatalf("plan was cut: %v", resp.Metadata)
	}
	if took := time.Since(start); took < 900*time.Millisecond {
		t.Fatalf("plan took %s; the test needs it to outlive 3 idle windows", took)
	}
}

// A single model call longer than the idle window counts as activity while it is
// in flight on the step lease.
func TestExecute_LongInFlightModelCallIsActivity(t *testing.T) {
	t.Parallel()
	var s *Server
	caller := callerFunc(func(ctx context.Context, agentID string, h *domain.Handoff, _ string) (*domain.Handoff, error) {
		if _, err := s.LLMGateway.GenerateWithTools(ctx, stepLease(h), domain.UserPrompt("700 lines")); err != nil {
			return nil, err
		}
		return okHandoff(agentID), nil
	})
	s = watchdogServer(t, 1, config.PlanConfig{PlanIdleTimeoutMs: 150, PlanMaxDurationMs: 60000},
		caller, sleepyModel{d: 600 * time.Millisecond}) // 4 idle windows in one call
	resp, err := s.Execute(t.Context(), &pb.Handoff{Payload: &pb.Object{Data: []byte("go")}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.Metadata[PlanTimedOutKey] == "true" || resp.Metadata["_partial_plan"] == "true" {
		t.Fatalf("a long in-flight model call was read as idle: %v", resp.Metadata)
	}
}

// A step that makes no call at all leaves the plan silent; it is cut with the
// idle reason, reported as a timeout.
func TestExecute_SilentPlanIsCutWithTheIdleReason(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	caller := callerFunc(func(ctx context.Context, _ string, _ *domain.Handoff, _ string) (*domain.Handoff, error) {
		calls.Add(1)
		<-ctx.Done() // hangs without a single kernel call
		return nil, ctx.Err()
	})
	s := watchdogServer(t, 1, config.PlanConfig{PlanIdleTimeoutMs: 150, PlanMaxDurationMs: 60000}, caller, nil)
	start := time.Now()
	resp, err := s.Execute(t.Context(), &pb.Handoff{Payload: &pb.Object{Data: []byte("go")}})
	msg := requireCeilingTimeout(t, resp, err)
	if !strings.Contains(msg, "plan idle for") || !strings.Contains(msg, "execution.plan.plan_idle_timeout_ms") {
		t.Fatalf("want the idle reason, got %q", msg)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("idle cut took %s", took)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("SelfHealer retried into the cut plan: %d calls", n)
	}
}

// A plan that never stops making progress is still cut at the absolute cap, with
// the max-duration reason.
func TestExecute_MaxDurationCapCutsABusyPlan(t *testing.T) {
	t.Parallel()
	var s *Server
	caller := callerFunc(func(ctx context.Context, agentID string, h *domain.Handoff, _ string) (*domain.Handoff, error) {
		// Busy for ever: back-to-back model calls, each well inside the idle window.
		for {
			if _, err := s.LLMGateway.GenerateWithTools(ctx, stepLease(h), domain.UserPrompt("again")); err != nil {
				return nil, err
			}
		}
	})
	s = watchdogServer(t, 1, config.PlanConfig{PlanIdleTimeoutMs: 150, PlanMaxDurationMs: 400},
		caller, sleepyModel{d: 20 * time.Millisecond})
	resp, err := s.Execute(t.Context(), &pb.Handoff{Payload: &pb.Object{Data: []byte("go")}})
	msg := requireCeilingTimeout(t, resp, err)
	if !strings.Contains(msg, "plan exceeded max duration") || !strings.Contains(msg, "execution.plan.plan_max_duration_ms") {
		t.Fatalf("want the max-duration reason, got %q", msg)
	}
}
