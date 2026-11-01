package operator

import (
	"sync"
	"time"
)

// Login hardening (ADR-0130 1f; T-20261031-operator-login-has-no-brute-force-limiter-non).
//
// Login is the ONE unauthenticated operator RPC, so it is the one place an
// online password guess lands. The owner's values: a source is locked out
// after 5 failures within 15 minutes; a successful login clears it. The
// source is the peer address the kernel observed on the connection — never
// anything the request body says — because a limiter keyed on a caller-chosen
// field is a limiter the caller resets.
//
// A locked-out source is refused BEFORE the credential is evaluated, with
// ResourceExhausted rather than Unauthenticated, so an operator who typed the
// right password on the sixth try learns to wait rather than to doubt it.

const (
	// loginMaxFailures is the number of failures within loginFailureWindow
	// that locks a source out.
	loginMaxFailures = 5
	// loginFailureWindow is the sliding window failures are counted over, and
	// therefore the longest a lockout lasts after the last failure.
	loginFailureWindow = 15 * time.Minute
)

// loginLimiter is the per-source failure window. Safe for concurrent use.
type loginLimiter struct {
	mu       sync.Mutex
	failures map[string][]time.Time // source → failure instants, oldest first
	now      func() time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{failures: map[string][]time.Time{}, now: time.Now}
}

// prune drops failures older than the window. Caller holds mu.
func (l *loginLimiter) prune(source string, now time.Time) []time.Time {
	kept := l.failures[source][:0]
	for _, t := range l.failures[source] {
		if now.Sub(t) < loginFailureWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.failures, source)
		return nil
	}
	l.failures[source] = kept
	return kept
}

// Allow reports whether source may attempt a login now, and if not, how long
// until the oldest counted failure ages out.
func (l *loginLimiter) Allow(source string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	recent := l.prune(source, now)
	if len(recent) < loginMaxFailures {
		return true, 0
	}
	return false, loginFailureWindow - now.Sub(recent[0])
}

// Fail records one failed attempt from source and returns how many are now
// counted against it.
func (l *loginLimiter) Fail(source string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.prune(source, now)
	l.failures[source] = append(l.failures[source], now)
	return len(l.failures[source])
}

// Reset clears source after a successful login.
func (l *loginLimiter) Reset(source string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, source)
}

// LoginAttempt is what the kernel observes about one Login call, success or
// failure. It carries the username and source, never the password.
type LoginAttempt struct {
	Username string
	Source   string
	Success  bool
	// LockedOut is true when the attempt was refused by the limiter without
	// the credential being evaluated.
	LockedOut bool
	// Failures is the count now held against the source (0 after a success).
	Failures int
	At       time.Time
}

// LoginObserver receives every login attempt. ADR-0130 1f: a failed login is
// emitted as an observation when a hook exists; the service falls back to a
// structured WARN log when none does, so the event is never silent.
type LoginObserver interface {
	ObserveLogin(LoginAttempt)
}
