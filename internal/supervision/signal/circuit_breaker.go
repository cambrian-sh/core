package signal

import (
	"sync"
	"time"
)

// CircuitBreaker tracks invalid signal rates per agent ID within a sliding
// time window. When an agent exceeds the threshold, ShouldInhibit returns true
// and the caller should reject further signals from that agent.
// Shared between supervision/watcher (OSS) and premium/reactive (premium). ADR-0032.
type CircuitBreaker struct {
	threshold  int
	windowSecs int

	mu             sync.Mutex
	invalidSignals map[string][]time.Time
}

// NewCircuitBreaker creates a CircuitBreaker. threshold is the maximum number
// of invalid signals allowed within windowSecs before inhibition triggers.
func NewCircuitBreaker(threshold, windowSecs int) *CircuitBreaker {
	return &CircuitBreaker{
		threshold:      threshold,
		windowSecs:     windowSecs,
		invalidSignals: make(map[string][]time.Time),
	}
}

// RecordInvalidSignal records a failed signal attempt at the current time.
func (cb *CircuitBreaker) RecordInvalidSignal(agentID string) {
	cb.RecordAt(agentID, time.Now())
}

// RecordAt records a signal at a specific time (used in tests for determinism).
func (cb *CircuitBreaker) RecordAt(agentID string, at time.Time) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.invalidSignals[agentID] = append(cb.invalidSignals[agentID], at)
	cb.pruneLocked(agentID)
}

// ShouldInhibit returns true when the agent has reached or exceeded the threshold.
func (cb *CircuitBreaker) ShouldInhibit(agentID string) bool {
	return cb.Count(agentID) >= cb.threshold
}

// Count returns the number of invalid signals within the current window.
func (cb *CircuitBreaker) Count(agentID string) int {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.pruneLocked(agentID)
	return len(cb.invalidSignals[agentID])
}

// ResetInvalidSignals clears all recorded signals for an agent.
func (cb *CircuitBreaker) ResetInvalidSignals(agentID string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	delete(cb.invalidSignals, agentID)
}

func (cb *CircuitBreaker) pruneLocked(agentID string) {
	signals := cb.invalidSignals[agentID]
	if len(signals) == 0 {
		return
	}
	cutoff := time.Now().Add(-time.Duration(cb.windowSecs) * time.Second)
	kept := signals[:0]
	for _, ts := range signals {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	cb.invalidSignals[agentID] = kept
}
