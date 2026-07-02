package operator

import (
	"sort"
	"sync"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// PlanInFlight is the projected live state of one executing plan. It is the
// fold of the latest PlanStateChanged event for a plan. ADR-0047 D7.
type PlanInFlight struct {
	SessionID   string
	PlanID      string
	ActiveStep  int
	Status      string
	ActiveAgent string
	CostSoFar   float64
}

// Projection folds the operator feed into live ephemeral runtime state — today,
// the set of plans in flight. It is the kernel's read source for "Plans in
// Flight" without a hot-path PlanRegistry (ADR-0047 D7): the plane shares the
// kernel's lifetime, so the fold captures every plan from process start.
// In-flight visibility does not survive a kernel restart — by design, neither
// do the plans (they live in DAGExecutor goroutine memory).
type Projection struct {
	mu    sync.Mutex
	plans map[string]PlanInFlight // keyed by PlanID
}

// NewProjection returns an empty projection.
func NewProjection() *Projection {
	return &Projection{plans: make(map[string]PlanInFlight)}
}

// Apply folds one event. Non-plan events are ignored. A terminal plan event
// removes the plan from the in-flight set; any other plan event upserts the
// latest absolute state. Idempotent for absolute-state events (re-applying the
// same event yields the same set).
func (p *Projection) Apply(e domain.DomainEvent) {
	ps, ok := e.(domain.PlanStateChanged)
	if !ok {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if ps.Terminal {
		delete(p.plans, ps.PlanID)
		return
	}
	p.plans[ps.PlanID] = PlanInFlight{
		SessionID:   ps.SessionID,
		PlanID:      ps.PlanID,
		ActiveStep:  ps.ActiveStep,
		Status:      ps.Status,
		ActiveAgent: ps.ActiveAgent,
		CostSoFar:   ps.CostSoFar,
	}
}

// PlansInFlight returns the current in-flight plans, ordered by PlanID for a
// stable snapshot.
func (p *Projection) PlansInFlight() []PlanInFlight {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]PlanInFlight, 0, len(p.plans))
	for _, pl := range p.plans {
		out = append(out, pl)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PlanID < out[j].PlanID })
	return out
}
