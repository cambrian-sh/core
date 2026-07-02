package operator

import "sync"

// ExecutionControls is the control handle a live DAGExecutor registers for its
// session so operator commands can steer it (ADR-0047 D11 / Amendment A1.1). It
// holds behavior, not state — the projection owns state. Inject delivers an
// operator instruction into the running plan; the executor owns the
// pause→replan→hot-swap→resume mechanism (the plane has no plan/context).
type ExecutionControls interface {
	Pause()
	Resume()
	Inject(instruction string) error
}

// ExecutionControlHub maps a session id to the control handle of its live
// execution (ADR-0047 D11), generalizing the ApprovalHub rendezvous. The
// DAGExecutor registers at Execute-start and deregisters on completion; a
// command to an unregistered session fails cleanly ("no live execution").
type ExecutionControlHub struct {
	mu sync.Mutex
	m  map[string]ExecutionControls
}

// NewExecutionControlHub constructs an empty hub.
func NewExecutionControlHub() *ExecutionControlHub {
	return &ExecutionControlHub{m: make(map[string]ExecutionControls)}
}

// Register binds a session id to its live execution's controls.
func (h *ExecutionControlHub) Register(sessionID string, c ExecutionControls) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.m[sessionID] = c
}

// Deregister removes a session's controls (call on Execute completion).
func (h *ExecutionControlHub) Deregister(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.m, sessionID)
}

// Lookup returns the controls for a session, or (nil,false) if none is live.
func (h *ExecutionControlHub) Lookup(sessionID string) (ExecutionControls, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.m[sessionID]
	return c, ok
}
