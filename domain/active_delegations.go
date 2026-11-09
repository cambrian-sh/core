package domain

import (
	"context"
	"sync"
	"time"
)

// ActiveDelegations tracks the plan-step leases whose worker is waiting on its
// own delegation to the planner (ADR-0145 D14: a worker hands extra work to the
// planner through Execute with its step lease, and its step stays open until the
// nested run returns). T-20261109-a-plan-step-is-re-dispatched-to-a-legacy-agent:
// the dispatcher's per-step deadline fired while a composed worker waited on its
// delegate_to_planner call; the self-healer re-dispatched the step on the same
// lease (to a legacy agent, since the lease already carried a composition), the
// lease was re-bound to that agent, and the waiting worker's next call failed
// "session not found". A step waiting on its own delegation is still working: the
// dispatcher does not time it out while the delegation runs (and gives it a full
// step deadline after it returns), and never re-dispatches it.
//
// Written by the kernel only (Server.Execute on a worker delegation); nil records
// nothing and reports nothing active. Safe for concurrent use.
type ActiveDelegations struct {
	mu     sync.Mutex
	now    func() time.Time
	active map[LeaseID]int
	ended  map[LeaseID]time.Time
}

// NewActiveDelegations builds an empty tracker.
func NewActiveDelegations() *ActiveDelegations {
	return &ActiveDelegations{now: time.Now, active: map[LeaseID]int{}, ended: map[LeaseID]time.Time{}}
}

// endedRetention bounds how long the end time of a finished delegation is kept.
const endedRetention = 6 * time.Hour

// Begin marks lease as waiting on a delegation; the returned func ends it (call
// it exactly once, when the nested run returns).
func (a *ActiveDelegations) Begin(lease LeaseID) (end func()) {
	if a == nil || lease == "" {
		return func() {}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	for l, at := range a.ended {
		if now.Sub(at) > endedRetention {
			delete(a.ended, l)
		}
	}
	a.active[lease]++
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.active[lease] <= 1 {
				delete(a.active, lease)
			} else {
				a.active[lease]--
			}
			a.ended[lease] = a.now()
		})
	}
}

// Active reports whether lease is waiting on a delegation now.
func (a *ActiveDelegations) Active(lease LeaseID) bool {
	if a == nil || lease == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.active[lease] > 0
}

// LastEnded returns when lease's most recent delegation returned (zero when it
// never delegated or is still waiting).
func (a *ActiveDelegations) LastEnded(lease LeaseID) time.Time {
	if a == nil || lease == "" {
		return time.Time{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active[lease] > 0 {
		return time.Time{}
	}
	return a.ended[lease]
}

type parentRunsKey struct{}

// WithParentRun records, on a nested run's ctx, the run whose worker delegated
// it (ADR-0145 D14). Kernel-side only. The nested run's steps may cite the
// parent run's ledger entries as evidence when readable under the person's access
// (T-20261109-staged-planner-names-earlier-stage-ledger).
func WithParentRun(ctx context.Context, run RunID) context.Context {
	if run == "" {
		return ctx
	}
	prev := ParentRunsFrom(ctx)
	out := make([]RunID, 0, len(prev)+1)
	out = append(out, prev...)
	out = append(out, run)
	return context.WithValue(ctx, parentRunsKey{}, out)
}

// ParentRunsFrom returns the chain of delegating runs, outermost first.
func ParentRunsFrom(ctx context.Context) []RunID {
	v, _ := ctx.Value(parentRunsKey{}).([]RunID)
	return v
}
