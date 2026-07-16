package domain

import (
	"sync"
	"time"
)

// ExplorationBudget bounds provisional-agent exploration (ROUTE-06 / ADR-0069): a
// provisional agent may WIN at most N auctions per capability per sliding window before
// its unconditional Layer-2 bypass is withdrawn. It replaces the previously-unbounded
// provisional bypass, so exploration is granted but not indefinitely. Safe for
// concurrent use. A nil *ExplorationBudget always allows (unbounded — the arm-off
// behavior).
type ExplorationBudget struct {
	mu     sync.Mutex
	n      int
	window time.Duration
	wins   map[string][]time.Time // capability → timestamps of recent provisional wins
	// OnExhausted, if set, is called once each time a capability transitions to
	// budget-exhausted (for an operator-visible event). May be nil.
	OnExhausted func(capability string)
	// now is injectable for tests; defaults to time.Now.
	now func() time.Time
}

// NewExplorationBudget builds a budget of n wins per capability per window. n <= 0 or
// window <= 0 disables bounding (Allowed always true).
func NewExplorationBudget(n int, window time.Duration) *ExplorationBudget {
	return &ExplorationBudget{n: n, window: window, wins: make(map[string][]time.Time), now: time.Now}
}

func (b *ExplorationBudget) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// pruneLocked drops win timestamps older than the window for a capability.
func (b *ExplorationBudget) pruneLocked(cap string, now time.Time) []time.Time {
	cutoff := now.Add(-b.window)
	ts := b.wins[cap]
	kept := ts[:0]
	for _, t := range ts {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	b.wins[cap] = kept
	return kept
}

// Allowed reports whether a provisional agent may still bypass Layer 2 for this
// capability (fewer than n wins in the window). A nil budget or non-positive bound
// always allows.
func (b *ExplorationBudget) Allowed(cap string) bool {
	if b == nil || b.n <= 0 || b.window <= 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pruneLocked(cap, b.clock())) < b.n
}

// RecordWin registers a provisional-agent win for a capability. When this pushes the
// capability to its bound, OnExhausted fires once. No-op on a nil/disabled budget.
func (b *ExplorationBudget) RecordWin(cap string) {
	if b == nil || b.n <= 0 || b.window <= 0 {
		return
	}
	b.mu.Lock()
	now := b.clock()
	ts := b.pruneLocked(cap, now)
	wasAllowed := len(ts) < b.n
	b.wins[cap] = append(ts, now)
	nowExhausted := len(b.wins[cap]) >= b.n
	cb := b.OnExhausted
	b.mu.Unlock()

	if cb != nil && wasAllowed && nowExhausted {
		cb(cap)
	}
}
