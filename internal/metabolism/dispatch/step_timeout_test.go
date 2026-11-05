package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// blockingCaller is an agent that never answers: it returns only when the ctx it
// was handed expires, which is exactly what a wedged executor looks like.
type blockingCaller struct{ seenDeadline bool }

func (b *blockingCaller) CallAgent(ctx context.Context, agentID string, _ *domain.Handoff, _ string) (*domain.Handoff, error) {
	_, b.seenDeadline = ctx.Deadline()
	<-ctx.Done()
	return nil, ctx.Err()
}

func profiledDispatcher(caller AgentCaller, latencyMs int, mult float64, bufferMs int) *Dispatcher {
	cfg := config.DefaultConfig().Execution
	cfg.Plan.StepTimeoutMultiplier = mult
	cfg.Plan.StepTimeoutBaseBufferMs = bufferMs
	// These tests exercise the bare ADR-0001 D5 formula in milliseconds; the 3-min
	// floor would swallow it. The floor has its own tests below.
	cfg.Plan.StepTimeoutFloorMs = 0
	return &Dispatcher{
		Gatekeeper: &fakeGatekeeper{candidates: []domain.ScoredCandidate{cand("slow", 0.9)}},
		Caller:     caller,
		Profiles: &fakeProfiles{byID: map[string]*domain.AgentProfile{
			"slow": {ComputationLatencyMedianMs: latencyMs},
		}},
		ExecCfg: cfg,
	}
}

// ADR-0001 D5: a step that outruns (latency*multiplier)+buffer is cut off with a
// deadline error, and the error names the step timeout rather than the agent.
func TestExecute_StepExceedingTimeoutReturnsDeadlineError(t *testing.T) {
	caller := &blockingCaller{}
	d := profiledDispatcher(caller, 10, 2.0, 20) // 10ms*2 + 20ms = 40ms

	start := time.Now()
	_, err := d.Execute(context.Background(), &domain.DispatchTask{ID: "t"}, &domain.Handoff{})
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
	if err == nil || !contains(err.Error(), "step timed out") {
		t.Fatalf("error should name the step timeout, got %v", err)
	}
	if !caller.seenDeadline {
		t.Fatal("agent ctx carried no deadline")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("step ran %s; the per-step deadline did not fire", elapsed)
	}
}

// The parent ctx expiring is the PLAN ceiling, not the step deadline: it must not
// be relabelled as a step timeout.
func TestExecute_ParentCancelIsNotAStepTimeout(t *testing.T) {
	caller := &blockingCaller{}
	d := profiledDispatcher(caller, 10_000, 2.0, 60_000)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := d.Execute(ctx, &domain.DispatchTask{ID: "t"}, &domain.Handoff{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
	if contains(err.Error(), "step timed out") {
		t.Fatalf("plan-ceiling expiry mislabelled as step timeout: %v", err)
	}
}

// A winner with no observed latency gets no per-step deadline: the formula has
// nothing to scale and a bare buffer would kill every first-run step.
func TestStepTimeout_NoProfileMeansNoDeadline(t *testing.T) {
	cfg := config.DefaultConfig().Execution
	d := &Dispatcher{ExecCfg: cfg}
	if got := d.stepTimeout(context.Background(), cand("a", 1)); got != 0 {
		t.Fatalf("nil Profiles: want 0, got %s", got)
	}
	d.Profiles = &fakeProfiles{byID: map[string]*domain.AgentProfile{"a": {}}}
	if got := d.stepTimeout(context.Background(), cand("a", 1)); got != 0 {
		t.Fatalf("profile without latency: want 0, got %s", got)
	}
	d.Profiles = &fakeProfiles{byID: map[string]*domain.AgentProfile{
		"a": {NetworkLatencyMedianMs: 100, ComputationLatencyMedianMs: 400},
	}}
	cfg.Plan.StepTimeoutMultiplier, cfg.Plan.StepTimeoutBaseBufferMs = 2.0, 15000
	cfg.Plan.StepTimeoutFloorMs = 0
	d.ExecCfg = cfg
	if got, want := d.stepTimeout(context.Background(), cand("a", 1)), 16*time.Second; got != want {
		t.Fatalf("want %s, got %s", want, got)
	}
	cfg.Plan.StepTimeoutMultiplier, cfg.Plan.StepTimeoutBaseBufferMs = 0, 0
	d.ExecCfg = cfg
	if got := d.stepTimeout(context.Background(), cand("a", 1)); got != 0 {
		t.Fatalf("both settings off: want 0, got %s", got)
	}
}

// delayedCaller is a healthy but slow agent: it answers after delay unless its ctx
// ends first. It records the deadline it was handed.
type delayedCaller struct {
	delay    time.Duration
	deadline time.Time
}

func (c *delayedCaller) CallAgent(ctx context.Context, agentID string, _ *domain.Handoff, _ string) (*domain.Handoff, error) {
	c.deadline, _ = ctx.Deadline()
	select {
	case <-time.After(c.delay):
		return &domain.Handoff{FromAgent: agentID, Payload: &domain.Payload{Data: []byte("done")}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// The floor (execution.plan.step_timeout_floor_ms) keeps a step alive past what
// a short observed median would allow: the formula gives 10ms*2+20ms = 40 ms, the
// agent needs 150 ms, the floor is 2 s — the step succeeds, and the deadline it
// ran under is the floor and is stamped on the DispatchTask
// (T-20261105-slow-model-reliability-hardcoded-20-s).
func TestExecute_StepTimeoutFloorKeepsASlowHealthyStepAlive(t *testing.T) {
	caller := &delayedCaller{delay: 150 * time.Millisecond}
	d := profiledDispatcher(caller, 10, 2.0, 20)
	d.ExecCfg.Plan.StepTimeoutFloorMs = 2000

	task := &domain.DispatchTask{ID: "t"}
	start := time.Now()
	res, err := d.Execute(context.Background(), task, &domain.Handoff{})
	if err != nil {
		t.Fatalf("a step within the floor must succeed, got %v", err)
	}
	if res == nil || res.Handoff == nil || string(res.Handoff.Payload.Data) != "done" {
		t.Fatalf("unexpected result %+v", res)
	}
	want := start.Add(2 * time.Second)
	if d := caller.deadline.Sub(want); d < -100*time.Millisecond || d > 100*time.Millisecond {
		t.Fatalf("agent ctx deadline %s, want ≈ now+floor (%s)", caller.deadline, want)
	}
	if !task.Deadline.Equal(caller.deadline) {
		t.Fatalf("DispatchTask.Deadline = %s, want the deadline the step ran under (%s)", task.Deadline, caller.deadline)
	}
}

// The floor raises a deadline, it does not shorten a long one, and it never
// creates one for a winner without a latency profile.
func TestStepTimeout_FloorRaisesButNeverCreates(t *testing.T) {
	cfg := config.DefaultConfig().Execution
	if cfg.Plan.StepTimeoutFloorMs != 180000 {
		t.Fatalf("default step_timeout_floor_ms = %d, want 180000", cfg.Plan.StepTimeoutFloorMs)
	}
	d := &Dispatcher{ExecCfg: cfg, Profiles: &fakeProfiles{byID: map[string]*domain.AgentProfile{
		"fast": {ComputationLatencyMedianMs: 5000},   // 5s*2+15s = 25s  → floor 180s
		"slow": {ComputationLatencyMedianMs: 120000}, // 120s*2+15s = 255s → formula
		"new":  {},
	}}}
	if got := d.stepTimeout(context.Background(), cand("fast", 1)); got != 180*time.Second {
		t.Fatalf("fast: want the 180s floor, got %s", got)
	}
	if got := d.stepTimeout(context.Background(), cand("slow", 1)); got != 255*time.Second {
		t.Fatalf("slow: want the 255s formula, got %s", got)
	}
	if got := d.stepTimeout(context.Background(), cand("new", 1)); got != 0 {
		t.Fatalf("unprofiled: want no per-step deadline, got %s", got)
	}
}

// With no per-step deadline the task carries the plan ceiling the ctx carries.
func TestExecute_TaskDeadlineIsThePlanCeilingWhenNoStepDeadline(t *testing.T) {
	caller := &delayedCaller{delay: time.Millisecond}
	d := profiledDispatcher(caller, 0, 2.0, 20) // no latency ⇒ no step deadline
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	planDL, _ := ctx.Deadline()
	task := &domain.DispatchTask{ID: "t", Deadline: time.Now().Add(20 * time.Second)}
	if _, err := d.Execute(ctx, task, &domain.Handoff{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !task.Deadline.Equal(planDL) {
		t.Fatalf("DispatchTask.Deadline = %s, want the plan ceiling %s", task.Deadline, planDL)
	}
}
