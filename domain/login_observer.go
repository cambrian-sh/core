package domain

import "time"

// LoginAttempt is what the kernel observes about one operator Login call,
// success or failure (ADR-0130 1f). It carries the username and the peer
// source, never the password.
//
// Declared in domain rather than in internal/substrate/operator so a plugin in
// another module can implement LoginObserver: Go forbids importing core's
// internal packages from cambrian-premium, and app.Registry seams take domain
// ports (ADR-0074/0082).
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

// LoginObserver receives every operator login attempt. ADR-0130 1f: a failed
// login is recorded as an observation (the premium receipts plugin chains it)
// in addition to the kernel's structured log line.
//
// Observation, never enforcement: the per-source limiter runs whether or not
// an observer exists, so a missing observer weakens the record, not the gate
// (AGENTS.md §2.3). Add-many, like DecisionObserver. The implementation MUST
// return promptly and MUST NOT panic — it is called synchronously inside the
// Login RPC.
type LoginObserver interface {
	ObserveLogin(LoginAttempt)
}

// MultiLoginObserver fans one attempt out to several observers, so the call
// site stays a single nil check.
type MultiLoginObserver []LoginObserver

func (m MultiLoginObserver) ObserveLogin(a LoginAttempt) {
	for _, o := range m {
		if o != nil {
			o.ObserveLogin(a)
		}
	}
}
