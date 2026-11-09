package dispatch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// T-20261109-a-plan-step-is-re-dispatched-to-a-legacy-agent (records gate replay,
// run f278c4d74121d508): a composed worker called delegate_to_planner; the step's
// 3-minute deadline fired while the nested run worked; the self-healer re-dispatched
// the step on the same lease, the lease was re-bound to data-analyst, and the
// waiting worker's next call failed "session not found".

type recordingBinder struct {
	mu    sync.Mutex
	binds []string
}

func (r *recordingBinder) BindLeaseAgent(_ domain.LeaseID, agentID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.binds = append(r.binds, agentID)
	return true
}

type delegatingCaller struct {
	delegs *domain.ActiveDelegations
	lease  domain.LeaseID
	hold   time.Duration
}

func (c *delegatingCaller) CallAgent(ctx context.Context, _ string, _ *domain.Handoff, _ string) (*domain.Handoff, error) {
	end := c.delegs.Begin(c.lease)
	go func() {
		time.Sleep(c.hold)
		end()
	}()
	<-ctx.Done()
	return nil, ctx.Err()
}

func leaseHandoff(lease string) *domain.Handoff {
	return &domain.Handoff{Context: map[string]string{handoffLeaseKey: lease}}
}

// The step deadline does not fire while the step's worker waits on its own
// delegation, and the worker gets a full step timeout after the delegation returns.
func TestExecute_StepDeadlineWaitsForTheWorkersDelegation(t *testing.T) {
	delegs := domain.NewActiveDelegations()
	// The worker, once called, delegates to the planner for 200ms and then waits.
	caller := &delegatingCaller{delegs: delegs, lease: "lease-1", hold: 200 * time.Millisecond}
	d := profiledDispatcher(caller, 10, 2.0, 20) // 40ms step timeout
	d.Delegations = delegs
	start := time.Now()
	_, err := d.Execute(context.Background(), &domain.DispatchTask{ID: "task-1"}, leaseHandoff("lease-1"))
	elapsed := time.Since(start)
	if elapsed < 200*time.Millisecond {
		t.Fatalf("the step was timed out after %s while its worker waited on its delegation: %v", elapsed, err)
	}
	if !errors.Is(err, context.DeadlineExceeded) || !contains(err.Error(), "step timed out") {
		t.Fatalf("after the delegation returned the step deadline should still fire: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the step deadline never fired after the delegation (%s)", elapsed)
	}
}

// A step whose worker is waiting on its own delegation is never re-dispatched, and
// its lease is not handed to another agent.
func TestExecute_RefusesToRedispatchAStepWaitingOnItsDelegation(t *testing.T) {
	caller := &fakeCaller{}
	binder := &recordingBinder{}
	d := newDispatcher(&fakeGatekeeper{candidates: []domain.ScoredCandidate{cand("data-analyst", 0.5)}}, caller)
	d.Leases = binder
	d.Delegations = domain.NewActiveDelegations()
	end := d.Delegations.Begin("lease-1")
	defer end()
	_, err := d.Execute(context.Background(), &domain.DispatchTask{ID: "task-1"}, leaseHandoff("lease-1"))
	if !errors.Is(err, ErrStepDelegating) {
		t.Fatalf("want ErrStepDelegating, got %v", err)
	}
	if len(caller.called) != 0 || len(binder.binds) != 0 {
		t.Fatalf("re-dispatched anyway: called %v, lease bound to %v", caller.called, binder.binds)
	}
	// Another step's lease dispatches normally.
	if _, err := d.Execute(context.Background(), &domain.DispatchTask{ID: "task-2"}, leaseHandoff("lease-2")); err != nil {
		t.Fatalf("an unrelated step was refused: %v", err)
	}
}
