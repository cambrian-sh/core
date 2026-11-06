package executer

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/metabolism/dispatch"
	"github.com/cambrian-sh/core/internal/supervision/aggregator"
)

// slowCaller answers after a fixed delay, and tries to spoof the latency stamp —
// the Dispatcher must overwrite it with what it measured.
type slowCaller struct{ delay time.Duration }

func (c slowCaller) CallAgent(_ context.Context, agentID string, _ *domain.Handoff, _ string) (*domain.Handoff, error) {
	time.Sleep(c.delay)
	return &domain.Handoff{
		FromAgent: agentID,
		Payload:   &domain.Payload{Data: []byte("answer")},
		Context:   map[string]string{domain.HandoffKeyWinnerLatencyMs: "999999"},
	}, nil
}

// T-20261104-step-taskevents-carry-no-latency-so-the-merit: a dispatched step's
// TaskEvent carries the wall time of the agent call, the stamp never leaks into the
// plan context, and the aggregator turns it into the profile latency routing reads.
// The same loop accumulates the ADR-0139 D6 outcome evidence: an unverified step is
// counted as unverified only; the verifier's score moves the verified evidence.
func TestDispatchedStepRecordsLatencyAndOutcomeEvidence(t *testing.T) {
	agent := domain.AgentDefinition{ID: "coder", SourceHash: "h-1"}
	d := &dispatch.Dispatcher{
		Gatekeeper: oneCandidateGatekeeper{agent: agent},
		Caller:     slowCaller{delay: 30 * time.Millisecond},
		ExecCfg:    config.DefaultConfig().Execution,
	}
	events := &eventStore{}
	exec := &DAGExecutor{EventWriter: events}
	plan := &domain.ExecutionPlan{Steps: []domain.Step{{Query: "q", RequiredCapabilities: []string{"code"}}}}
	stepFn := func(ctx context.Context, i int, h *domain.Handoff) (*domain.Handoff, error) {
		res, err := d.Execute(ctx, &domain.DispatchTask{ID: "task-0", Description: plan.Steps[i].Query}, h)
		if err != nil {
			return nil, err
		}
		return res.Handoff, nil
	}
	final, err := exec.Execute(t.Context(), plan, nil, stepFn)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for k := range final {
		if strings.HasSuffix(k, domain.HandoffKeyWinnerLatencyMs) {
			t.Fatalf("latency stamp leaked into the plan context as %q", k)
		}
	}

	events.mu.Lock()
	var ev domain.TaskEvent
	for _, e := range events.events {
		ev = e
	}
	events.mu.Unlock()
	if ev.ComputationLatencyMs < 30 || ev.ComputationLatencyMs >= 999999 {
		t.Fatalf("TaskEvent latency = %d ms, want the measured >= 30 ms (not the agent's spoofed value)", ev.ComputationLatencyMs)
	}

	profiles := &profileMap{}
	agg := aggregator.New(events, profiles, aggregator.AggregatorConfig{EWMAAlpha: 0.3, LatencyWindow: 10})
	if err := agg.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	p, _ := profiles.GetProfile(context.Background(), agent.ID, agent.SourceHash)
	if p == nil || p.ComputationLatencyMedianMs != ev.ComputationLatencyMs {
		t.Fatalf("profile latency = %+v, want median %d", p, ev.ComputationLatencyMs)
	}
	if p.Outcomes != (domain.OutcomeEvidence{Unverified: 1}) {
		t.Fatalf("unverified-only global evidence = %+v, want {Unverified:1}", p.Outcomes)
	}
	if got := p.CapabilityStats["code"].Outcomes; got != (domain.OutcomeEvidence{Unverified: 1}) {
		t.Fatalf("unverified-only capability evidence = %+v, want {Unverified:1}", got)
	}

	events.mu.Lock()
	var ids []string
	for id := range events.events {
		ids = append(ids, id)
	}
	events.mu.Unlock()
	for _, id := range ids {
		events.markVerified(id, 0.75)
	}
	if err := agg.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	p, _ = profiles.GetProfile(context.Background(), agent.ID, agent.SourceHash)
	want := domain.OutcomeEvidence{VerifiedSuccess: 0.75, VerifiedFailure: 0.25}
	if p.CapabilityStats["code"].Outcomes != want || p.Outcomes != want {
		t.Fatalf("verified evidence = global %+v / code %+v, want %+v", p.Outcomes, p.CapabilityStats["code"].Outcomes, want)
	}
}

// A response whose stamp names another agent (a runner-up fallback) must not borrow
// the winner's latency either.
func TestDAGExecutor_WinnerLatencyIgnoredWhenAgentDiffers(t *testing.T) {
	mw := &mockEventWriter{}
	d := &DAGExecutor{EventWriter: mw}
	plan := &domain.ExecutionPlan{Steps: []domain.Step{{Query: "q"}}}
	stepFn := func(context.Context, int, *domain.Handoff) (*domain.Handoff, error) {
		return &domain.Handoff{FromAgent: "runner-up", Payload: &domain.Payload{Data: []byte("x")}, Context: map[string]string{
			domain.HandoffKeyWinnerAgentID:   "winner",
			domain.HandoffKeyWinnerLatencyMs: "1234",
		}}, nil
	}
	if _, err := d.Execute(t.Context(), plan, nil, stepFn); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	mw.mu.Lock()
	defer mw.mu.Unlock()
	if len(mw.events) != 1 || mw.events[0].ComputationLatencyMs != 0 {
		t.Fatalf("events = %+v, want one event with unknown (0) latency", mw.events)
	}
}
