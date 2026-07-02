package domain

import "sync"

// RunGrantOverlay holds tool grants conferred run-scoped by a loaded system skill
// (ADR-0046 D6), keyed by the run's session token. It is the ephemeral half of
// the skill grant model: a system skill (operator-authored) may confer tools the
// agent otherwise lacks for the duration of the run; Clear drops them at task end.
//
// Only system skills reach this overlay — agent-local skills can grant only tools
// already in the agent's static envelope (narrow-only), so they need no overlay.
// Conferring is therefore always an operator-authorized widening; dangerous tools
// still require approval at execute time (the overlay grants admission, not a
// bypass). Concurrency-safe; all methods are nil-receiver-safe.
type RunGrantOverlay struct {
	mu        sync.RWMutex
	bySession map[string]map[string]struct{}
}

// NewRunGrantOverlay constructs an empty overlay.
func NewRunGrantOverlay() *RunGrantOverlay {
	return &RunGrantOverlay{bySession: map[string]map[string]struct{}{}}
}

// Activate confers tools to a run (session). No-op on a nil overlay / empty session.
func (o *RunGrantOverlay) Activate(session string, tools []string) {
	if o == nil || session == "" || len(tools) == 0 {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	set := o.bySession[session]
	if set == nil {
		set = make(map[string]struct{}, len(tools))
		o.bySession[session] = set
	}
	for _, t := range tools {
		set[t] = struct{}{}
	}
}

// Granted reports whether tool was conferred to the run (session).
func (o *RunGrantOverlay) Granted(session, tool string) bool {
	if o == nil || session == "" {
		return false
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	_, ok := o.bySession[session][tool]
	return ok
}

// Clear drops all grants conferred to a run (session) — the ephemerality backstop,
// called at task end so a conferred capability never outlives the run.
func (o *RunGrantOverlay) Clear(session string) {
	if o == nil || session == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.bySession, session)
}
