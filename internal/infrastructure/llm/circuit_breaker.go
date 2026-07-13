package llm

import (
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// CircuitBreaker is the per-id health authority for the LLM Provider (ADR-0042
// D4). It is a passive, traffic-driven circuit breaker: it learns health only
// from the outcomes the Provider already routes — no active probing.
//
// State per id:
//
//	healthy --(>= threshold consecutive failures)--> OPEN
//	OPEN    --(cooldown elapsed)-------------------> half-open (Healthy returns true to permit one probe)
//	half-open --(Record ok)--> healthy   |   --(Record fail)--> OPEN (cooldown resets)
//
// Healthy is read-only; Record drives all state transitions. Because the
// Provider only issues a call when Healthy reports true, a Record arriving while
// an id is OPEN is necessarily a half-open probe result.
type CircuitBreaker struct {
	mu        sync.Mutex
	threshold int
	cooldown  time.Duration
	now       func() time.Time
	entries   map[string]*breakerEntry
	// Bus is optional (ADR-0047 D3): when set, an open↔closed transition publishes
	// an LLMHealthEvent for the operator feed. nil ⇒ no-op.
	Bus domain.EventBus
}

type breakerEntry struct {
	consecutiveFailures int
	open                bool
	openedAt            time.Time
}

// NewCircuitBreaker constructs a breaker. A threshold < 1 is clamped to 1.
func NewCircuitBreaker(threshold int, cooldown time.Duration) *CircuitBreaker {
	return newCircuitBreakerWithClock(threshold, cooldown, time.Now)
}

// newCircuitBreakerWithClock is the test seam: an injectable clock makes
// cooldown transitions deterministic without sleeping.
func newCircuitBreakerWithClock(threshold int, cooldown time.Duration, now func() time.Time) *CircuitBreaker {
	if threshold < 1 {
		threshold = 1
	}
	return &CircuitBreaker{
		threshold: threshold,
		cooldown:  cooldown,
		now:       now,
		entries:   make(map[string]*breakerEntry),
	}
}

// Healthy reports whether the id may currently receive traffic. An unknown id is
// optimistically healthy. An OPEN id becomes permittable (half-open) once the
// cooldown has elapsed, allowing exactly the probe that decides recovery.
func (b *CircuitBreaker) Healthy(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	e := b.entries[id]
	if e == nil || !e.open {
		return true
	}
	return b.now().Sub(e.openedAt) >= b.cooldown
}

// Record feeds a call outcome (ok=false covers transport errors, non-200,
// timeouts, and empty/unparseable responses — see the health decorator). It
// trips the circuit at the failure threshold and handles half-open recovery.
func (b *CircuitBreaker) Record(id string, ok bool) {
	state, changed := b.recordLocked(id, ok)
	// Publish outside the lock (EventBus delivery is synchronous; never hold the
	// breaker lock across handler calls).
	if changed && b.Bus != nil {
		_ = b.Bus.Publish(domain.LLMHealthEvent{ModelID: id, State: state})
	}
}

// recordLocked applies the outcome and reports whether the open/closed state
// changed, with the absolute new state ("open" | "closed").
func (b *CircuitBreaker) recordLocked(id string, ok bool) (state string, changed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	e := b.entries[id]
	if e == nil {
		e = &breakerEntry{}
		b.entries[id] = e
	}
	wasOpen := e.open

	if ok {
		e.consecutiveFailures = 0
		e.open = false
	} else {
		e.consecutiveFailures++
		// Trip when the threshold is reached from healthy, OR re-open immediately
		// when a half-open probe fails (the id was already OPEN). Either way the
		// cooldown clock restarts from now.
		if e.open || e.consecutiveFailures >= b.threshold {
			e.open = true
			e.openedAt = b.now()
		}
	}

	if e.open == wasOpen {
		return "", false
	}
	if e.open {
		return "open", true
	}
	return "closed", true
}
