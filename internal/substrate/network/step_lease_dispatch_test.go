package network

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/internal/metabolism/dispatch"
)

// ADR-0139 D8 / T-20261104-plan-step-leases-are-bound-with-no-agentid-so: a plan-step
// lease is minted before selection, so it is bound to the executing agent by the
// dispatcher, immediately before each CallAgent. These tests drive the REAL dispatcher
// against the REAL gateway and resolve the lease through the same Server.resolveLeaseFor
// every transport lane uses.

type stepLeaseGatekeeper struct{ candidates []domain.ScoredCandidate }

func (g stepLeaseGatekeeper) FindCandidates(context.Context, *domain.DispatchTask) ([]domain.ScoredCandidate, error) {
	return g.candidates, nil
}

func (g stepLeaseGatekeeper) FindModelCandidates(context.Context, []string) ([]domain.ScoredCandidate, error) {
	return nil, nil
}

// leaseProbeCaller records, at the moment each agent is called, the lease's binding and
// whether the called agent — and an outsider — can resolve it.
type leaseProbeCaller struct {
	s       *Server
	gw      *SubstrateLLMGateway
	outside string
	fail    map[string]bool
	seen    []leaseProbe
}

type leaseProbe struct {
	agent        string
	boundAgent   string
	selfResolves bool
	outsiderOK   bool
}

func (c *leaseProbeCaller) CallAgent(_ context.Context, agentID string, h *domain.Handoff, _ string) (*domain.Handoff, error) {
	lease := domain.LeaseID(h.Context["_session_token_id"])
	b, _ := c.gw.ResolveLease(lease)
	_, self := c.s.resolveLeaseFor(agentPlaneCtx(agentID), lease)
	_, outsider := c.s.resolveLeaseFor(agentPlaneCtx(c.outside), lease)
	c.seen = append(c.seen, leaseProbe{agent: agentID, boundAgent: b.AgentID, selfResolves: self, outsiderOK: outsider})
	if c.fail[agentID] {
		return nil, errors.New("agent failed")
	}
	return &domain.Handoff{FromAgent: agentID}, nil
}

func stepLeaseFixture(t *testing.T, fail map[string]bool) (*Server, *SubstrateLLMGateway, *dispatch.Dispatcher, *leaseProbeCaller, domain.LeaseID, *domain.Handoff) {
	t.Helper()
	s, gw := newLeaseServer(t)
	caller := &leaseProbeCaller{s: s, gw: gw, outside: "agent-intruder", fail: fail}
	d := &dispatch.Dispatcher{
		Gatekeeper: stepLeaseGatekeeper{candidates: []domain.ScoredCandidate{
			{Agent: domain.AgentDefinition{ID: "agent-winner"}, Score: 0.9},
			{Agent: domain.AgentDefinition{ID: "agent-runnerup"}, Score: 0.7},
		}},
		Caller:  caller,
		ExecCfg: config.DefaultConfig().Execution,
		Leases:  gw,
	}
	// Exactly what the step path does (server.go): Acquire, then bind with no AgentID.
	lease, err := gw.Acquire(context.Background(), domain.StepAllocation{}, StepLeaseTokenLimit, time.Minute)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	gw.BindLease(lease, domain.LeaseBinding{SessionID: "sess-1", RunID: "run-1", StepIndex: 3})
	h := &domain.Handoff{Context: map[string]string{"_session_token_id": string(lease)}}
	return s, gw, d, caller, lease, h
}

func TestStepLease_BoundToWinnerAtDispatch(t *testing.T) {
	t.Parallel()
	s, gw, d, caller, lease, h := stepLeaseFixture(t, nil)

	if _, err := d.Execute(context.Background(), &domain.DispatchTask{ID: "task-3"}, h); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(caller.seen) != 1 {
		t.Fatalf("want one call, got %+v", caller.seen)
	}
	p := caller.seen[0]
	if p.boundAgent != "agent-winner" {
		t.Errorf("lease AgentID at call time = %q, want agent-winner — the agent got an unbound lease", p.boundAgent)
	}
	if !p.selfResolves {
		t.Error("the winner must resolve its own step lease")
	}
	if p.outsiderOK {
		t.Error("an agent the step was not dispatched to resolved the step lease")
	}

	// The rest of the binding survives the agent bind.
	b, _ := gw.ResolveLease(lease)
	if b.SessionID != "sess-1" || b.RunID != "run-1" || b.StepIndex != 3 || b.AgentID != "agent-winner" {
		t.Errorf("binding after dispatch = %+v", b)
	}
	// Every resolution lane refuses the outsider, including the handoff payload lane.
	ctxB := agentPlaneCtx("agent-intruder", leaseHeader, string(lease))
	if sid := s.resolveCallerSession(ctxB); sid != "" {
		t.Errorf("outsider resolved the step's session %q", sid)
	}
	if !s.leaseForeignToCaller(ctxB, lease) {
		t.Error("the step lease must read as foreign to an outsider")
	}
	if _, ok := s.resolveBindingFromHandoff(agentPlaneCtx("agent-intruder"), map[string]string{"_session_token_id": string(lease)}); ok {
		t.Error("handoff lane: outsider got the step binding")
	}
}

// Runner-up fallback (server.go runFallback → Dispatcher.CallAgent) hands the SAME lease
// to the runner-up: it must now be bound to the runner-up, and the displaced winner
// refused.
func TestStepLease_FallbackRebindsToRunnerUp(t *testing.T) {
	t.Parallel()
	s, _, d, caller, lease, h := stepLeaseFixture(t, map[string]bool{"agent-winner": true})

	res, err := d.Execute(context.Background(), &domain.DispatchTask{ID: "task-3"}, h)
	if err == nil {
		t.Fatal("winner was set to fail")
	}
	if res == nil || len(res.RunnerUps) != 1 {
		t.Fatalf("runner-ups = %+v", res)
	}
	if _, err := d.CallAgent(context.Background(), res.RunnerUps[0].Agent.ID, h, ""); err != nil {
		t.Fatalf("fallback call: %v", err)
	}
	if len(caller.seen) != 2 {
		t.Fatalf("want winner then runner-up, got %+v", caller.seen)
	}
	fb := caller.seen[1]
	if fb.agent != "agent-runnerup" || fb.boundAgent != "agent-runnerup" || !fb.selfResolves {
		t.Errorf("runner-up was handed a lease it cannot use: %+v", fb)
	}
	if _, ok := s.resolveLeaseFor(agentPlaneCtx("agent-winner"), lease); ok {
		t.Error("the displaced winner still resolves the step lease after fallback")
	}
	if _, ok := s.resolveLeaseFor(agentPlaneCtx("agent-intruder"), lease); ok {
		t.Error("an outsider resolves the step lease after fallback")
	}
}

// The operator plane is not an agent: a dispatched step lease still resolves for it.
func TestStepLease_OperatorStillResolvesAfterDispatch(t *testing.T) {
	t.Parallel()
	s, _, d, _, lease, h := stepLeaseFixture(t, nil)
	if _, err := d.Execute(context.Background(), &domain.DispatchTask{ID: "task-3"}, h); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if sid := s.resolveCallerSession(operatorPlaneCtx(leaseHeader, string(lease))); sid != "sess-1" {
		t.Errorf("operator resolution broke, got %q", sid)
	}
}

// BindLeaseAgent never resurrects a completed lease and ignores empty input.
func TestBindLeaseAgent_UnknownOrCompletedLease(t *testing.T) {
	t.Parallel()
	_, gw := newLeaseServer(t)
	if gw.BindLeaseAgent("no-such-lease", "a") {
		t.Error("unknown lease reported as bound")
	}
	lease, _ := gw.Acquire(context.Background(), domain.StepAllocation{}, 16, time.Minute)
	if gw.BindLeaseAgent(lease, "") {
		t.Error("empty agent id reported as bound")
	}
	if _, err := gw.Complete(context.Background(), lease); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if gw.BindLeaseAgent(lease, "a") {
		t.Error("completed lease reported as bound")
	}
	if _, known := gw.ResolveLease(lease); known {
		t.Error("BindLeaseAgent resurrected a completed lease")
	}
}

// Retrieval spend is charged to the caller's parent lease only when the caller holds it:
// an agent cannot bill its retrieval chain to a victim's step lease.
func TestCallerLease_RefusesALeaseBoundToAnotherAgent(t *testing.T) {
	t.Parallel()
	_, gw := newLeaseServer(t)
	victim, _ := gw.Acquire(context.Background(), domain.StepAllocation{}, 4096, time.Minute)
	gw.BindLease(victim, domain.LeaseBinding{SessionID: "victim", StepIndex: 0, AgentID: "agent-a"})

	if got := callerLease(agentPlaneCtx("agent-b", leaseHeader, string(victim)), gw); got != "" {
		t.Errorf("agent B may not charge agent A's lease, got %q", got)
	}
	if got := callerLease(agentPlaneCtx("agent-a", leaseHeader, string(victim)), gw); got != victim {
		t.Errorf("agent A's own lease must be chargeable, got %q", got)
	}
	// A value that is not a live lease passes through unchanged; ChargeLease ignores it.
	if got := callerLease(agentPlaneCtx("agent-b", sessionHeader, "not-a-lease"), gw); got != "not-a-lease" {
		t.Errorf("non-lease value = %q", got)
	}
}
