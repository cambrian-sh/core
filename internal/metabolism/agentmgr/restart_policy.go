package agentmgr

import (
	"math"
	"math/rand/v2"
	"sync"
	"time"
)

// DaemonRestartPolicy decides whether and how soon a crashed daemon is restarted, and
// quarantines a crash-looping one (REACT-04 / ADR-0070). It tracks restart attempts per
// stream within a sliding window; once the attempt count in the window reaches
// MaxAttempts the daemon is quarantined instead of restarted (the flap guard). Restart
// delay is exponential with full jitter, bounded by [Base, Max]. Concurrency-safe.
type DaemonRestartPolicy struct {
	MaxAttempts int
	Window      time.Duration
	Base        time.Duration
	Max         time.Duration

	mu       sync.Mutex
	attempts map[string][]time.Time
	// now / jitter are injectable for tests.
	now    func() time.Time
	jitter func(max time.Duration) time.Duration
}

// NewDaemonRestartPolicy builds a policy from config values. maxAttempts <= 0 disables
// auto-restart (Register always quarantines-as-"no restart" — the caller leaves the
// daemon down).
func NewDaemonRestartPolicy(maxAttempts int, window, base, max time.Duration) *DaemonRestartPolicy {
	if base <= 0 {
		base = time.Second
	}
	if max <= 0 {
		max = 30 * time.Second
	}
	return &DaemonRestartPolicy{
		MaxAttempts: maxAttempts,
		Window:      window,
		Base:        base,
		Max:         max,
		attempts:    make(map[string][]time.Time),
		now:         time.Now,
		jitter:      func(m time.Duration) time.Duration { return time.Duration(rand.Int64N(int64(m) + 1)) },
	}
}

// Register records a restart attempt for streamID and returns the delay to wait before
// restarting. quarantine=true means the flap limit was reached — do NOT restart; put the
// daemon in quarantine. A disabled policy (MaxAttempts <= 0) always quarantines.
func (p *DaemonRestartPolicy) Register(streamID string) (delay time.Duration, quarantine bool) {
	if p == nil || p.MaxAttempts <= 0 {
		return 0, true
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.now()
	kept := p.pruneLocked(streamID, now)
	if len(kept) >= p.MaxAttempts {
		return 0, true // crash-loop → quarantine
	}
	n := len(kept) // 0-based consecutive attempt index within the window
	p.attempts[streamID] = append(kept, now)

	// Full-jitter exponential backoff: delay ∈ [0, min(Max, Base·2^n)].
	ceiling := float64(p.Base) * math.Pow(2, float64(n))
	capped := time.Duration(math.Min(ceiling, float64(p.Max)))
	return p.jitter(capped), false
}

// Reset clears a stream's attempt history — call after a daemon has run healthily past
// the window, or on an explicit stop, so a later crash starts fresh.
func (p *DaemonRestartPolicy) Reset(streamID string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	delete(p.attempts, streamID)
	p.mu.Unlock()
}

func (p *DaemonRestartPolicy) pruneLocked(streamID string, now time.Time) []time.Time {
	if p.Window <= 0 {
		return p.attempts[streamID]
	}
	cutoff := now.Add(-p.Window)
	ts := p.attempts[streamID]
	kept := ts[:0]
	for _, t := range ts {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	p.attempts[streamID] = kept
	return kept
}
