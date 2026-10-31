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
