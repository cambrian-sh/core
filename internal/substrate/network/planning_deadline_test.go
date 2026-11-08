package network

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/cambrian-sh/core/api/proto"
	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// slowPlanner is a planner whose model call outlives any caller: it returns only
// when its ctx ends, as a provider call does when its request is cancelled.
type slowPlanner struct{ calls atomic.Int32 }

func (p *slowPlanner) GetExecutionPlan(ctx context.Context, _ string) (*domain.ExecutionPlan, error) {
	p.calls.Add(1)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (p *slowPlanner) Generate(ctx context.Context, _ string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

// slowRouter classifies only when its ctx ends (a Layer-3 model call that hangs).
type slowRouter struct{}

func (slowRouter) Resolve(ctx context.Context, _ domain.RouterInput) (*domain.RouterDecision, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func requirePlanningTimeout(t *testing.T, ctx context.Context, resp *pb.Handoff, err error, phase string) {
	t.Helper()
	if ctx.Err() != nil {
		t.Fatalf("Execute returned after its caller's deadline (%v); the answer would be lost on the wire", ctx.Err())
	}
	if err != nil {
		t.Fatalf("Execute: %v — want an in-time 'ran out of time while %s' answer, not an error", err, phase)
	}
	if resp.Metadata[PlanTimedOutKey] != "true" || resp.Metadata["_partial_plan"] != "true" {
		t.Fatalf("want a timed-out partial result, got metadata %v", resp.Metadata)
	}
	if c := resp.Metadata[PlanTimeoutCauseKey]; !strings.Contains(c, "execution.plan.caller_deadline_reserve_ms") {
		t.Errorf("cause %q does not name the reserve key", c)
	}
	payload := string(resp.Payload.Data)
	for _, want := range []string{"ran out of time while " + phase, "nothing was executed"} {
		if !strings.Contains(payload, want) {
			t.Errorf("payload lacks %q:\n%s", want, payload)
		}
	}
}

// The planner's model call ran under the bare caller ctx, outside the
// caller-deadline reserve that bounds the plan: a slow planner spent the caller's
// whole budget and the caller got DEADLINE_EXCEEDED with no payload. Planning now
// ends at the same reserve instant and Execute answers in time that it ran out of
// time while planning and executed nothing
// (T-20261107-planner-llm-call-is-not-bounded-by-the-caller).
func TestExecute_SlowPlannerIsStoppedByTheCallerReserveAndSaysNothingRan(t *testing.T) {
	t.Parallel()
	var dispatched atomic.Int32
	caller := callerFunc(func(context.Context, string, *domain.Handoff, string) (*domain.Handoff, error) {
		dispatched.Add(1)
		return okHandoff("worker"), nil
	})
	s := watchdogServer(t, 1, config.PlanConfig{PlanMaxDurationMs: 60000, CallerDeadlineReserveMs: 300}, caller, nil)
	planner := &slowPlanner{}
	s.Planner = planner

	ctx, cancel := context.WithTimeout(t.Context(), 1200*time.Millisecond)
	defer cancel()
	resp, err := s.Execute(ctx, &pb.Handoff{Payload: &pb.Object{Data: []byte("summarise the open cases")}})
	requirePlanningTimeout(t, ctx, resp, err, "planning")
	if planner.calls.Load() == 0 {
		t.Fatal("the planner was never called")
	}
	if n := dispatched.Load(); n != 0 {
		t.Fatalf("%d steps dispatched; a plan that never arrived must run nothing", n)
	}
}

// The router's Layer-3 classification is the other model call before the plan
// starts; it is held to the same reserve.
func TestExecute_SlowRouterIsStoppedByTheCallerReserve(t *testing.T) {
	t.Parallel()
	s := watchdogServer(t, 1, config.PlanConfig{PlanMaxDurationMs: 60000, CallerDeadlineReserveMs: 300}, nil, nil)
	s.Router = slowRouter{}
	planner := &slowPlanner{}
	s.Planner = planner

	ctx, cancel := context.WithTimeout(t.Context(), 1200*time.Millisecond)
	defer cancel()
	resp, err := s.Execute(ctx, &pb.Handoff{Payload: &pb.Object{Data: []byte("summarise the open cases")}})
	requirePlanningTimeout(t, ctx, resp, err, "classifying")
	if planner.calls.Load() != 0 {
		t.Fatal("the planner ran after the router was cut")
	}
}

// One reserve instant for every phase: the plan's deadline is fixed from the
// caller's whole budget when Execute starts, not re-derived from what planning
// left over.
func TestCallerReserve_FixedOncePerExecute(t *testing.T) {
	t.Parallel()
	parent, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pd, _ := parent.Deadline()
	r := newCallerReserve(parent, 5*time.Second) // capped at half: about 1s
	time.Sleep(50 * time.Millisecond)            // planning used some of the budget
	c, stop := r.bound(parent, errPlanCallerDeadline(r.reserve))
	defer stop()
	d, _ := c.Deadline()
	if !d.Equal(r.at) {
		t.Fatalf("bound deadline %v, want the fixed instant %v", d, r.at)
	}
	if got := pd.Sub(d); got < 900*time.Millisecond || got > 1100*time.Millisecond {
		t.Fatalf("reserve %s, want about half the caller's 2s", got)
	}
	if none := newCallerReserve(context.Background(), time.Second); none.ok {
		t.Fatal("a caller with no deadline has no reserve")
	}
}
