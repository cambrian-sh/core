package operator

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/cambrian-sh/core/domain"
)

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
	m  map[domain.SessionID]ExecutionControls
	// runs are the live Executes CancelRun can end (TrackRun).
	runs map[*trackedRun]struct{}
}

// NewExecutionControlHub constructs an empty hub.
func NewExecutionControlHub() *ExecutionControlHub {
	return &ExecutionControlHub{m: make(map[domain.SessionID]ExecutionControls)}
}

// Register binds a session id to its live execution's controls.
func (h *ExecutionControlHub) Register(sessionID domain.SessionID, c ExecutionControls) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.m[sessionID] = c
}

// Deregister removes a session's controls (call on Execute completion).
func (h *ExecutionControlHub) Deregister(sessionID domain.SessionID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.m, sessionID)
}

// Lookup returns the controls for a session, or (nil,false) if none is live.
func (h *ExecutionControlHub) Lookup(sessionID string) (ExecutionControls, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// The operator plane hands us a raw proto string; converting here keeps the wire
	// edge explicit and the hub's key typed.
	c, ok := h.m[domain.SessionID(sessionID)]
	return c, ok
}

// ── Terminal run cancellation (contract 0102, CancelRun) ─────────────────────
//
// T-20261106-operator-closesession-only-pauses-a-live-plan (owner decision
// 2026-11-14): CloseSession only PAUSES a live execution, so a closed session's
// plan stayed parked with its goroutine, step leases and approvals alive. CancelRun
// is the terminal stop. The hub tracks every live Execute — single-agent lane and
// planner lane alike — by session, with a cancel-cause func over the context the
// run executes under; cancelling it stops the executor, every in-flight step and
// the dispatch to the agent, and each lane's deferred lease completion then refuses
// any later tool call on the run's leases. The run owner (the network server)
// records the terminal "cancelled" status when it sees the cause (RunCancelled).

// RunCancelledError is the cancellation cause CancelRun installs: who cancelled
// the run and why. errors.Is(err, ErrRunCancelled) matches it.
type RunCancelledError struct {
	Actor  string
	Reason string
}

// ErrRunCancelled is the sentinel every RunCancelledError matches.
var ErrRunCancelled = errors.New("run cancelled by an operator")

func (e *RunCancelledError) Error() string {
	return fmt.Sprintf("run cancelled by %s: %s", e.Actor, e.Reason)
}

// Is makes errors.Is(cause, ErrRunCancelled) true.
func (e *RunCancelledError) Is(target error) bool { return target == ErrRunCancelled }

// trackedRun is one live Execute registered with the hub.
type trackedRun struct {
	session domain.SessionID
	cancel  context.CancelCauseFunc
	mu      sync.Mutex
	runIDs  []domain.RunID
}

func (t *trackedRun) hasRun(id domain.RunID) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, r := range t.runIDs {
		if r == id {
			return true
		}
	}
	return false
}

type trackedRunKey struct{}

// TrackRun registers a live Execute for sessionID and returns the context it
// must run under (cancelled by CancelRun) and the func to call when it returns.
// A nil hub or an empty session leaves ctx untracked. Nested Executes of one
// session (a worker delegating to the planner) are tracked separately, so a
// session-wide cancel reaches all of them.
func (h *ExecutionControlHub) TrackRun(ctx context.Context, sessionID domain.SessionID) (context.Context, func()) {
	if h == nil || sessionID == "" {
		return ctx, func() {}
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	t := &trackedRun{session: sessionID, cancel: cancel}
	h.mu.Lock()
	if h.runs == nil {
		h.runs = map[*trackedRun]struct{}{}
	}
	h.runs[t] = struct{}{}
	h.mu.Unlock()
	return context.WithValue(runCtx, trackedRunKey{}, t), func() {
		h.mu.Lock()
		delete(h.runs, t)
		h.mu.Unlock()
		cancel(nil)
	}
}

// AttachRunID names the run a tracked Execute is executing, once the lane has
// minted its id, so CancelRun can address it by run_id. No-op on an untracked ctx.
func AttachRunID(ctx context.Context, runID domain.RunID) {
	t, ok := ctx.Value(trackedRunKey{}).(*trackedRun)
	if !ok || runID == "" {
		return
	}
	t.mu.Lock()
	t.runIDs = append(t.runIDs, runID)
	t.mu.Unlock()
}

// RunCancelled reports whether ctx was ended by CancelRun, and the cause.
func RunCancelled(ctx context.Context) (*RunCancelledError, bool) {
	var rc *RunCancelledError
	if errors.As(context.Cause(ctx), &rc) {
		return rc, true
	}
	return nil, false
}

// CancelRun cancels every tracked Execute matching sessionID and/or runID (an
// empty field matches anything; both empty matches nothing) with cause, and
// reports how many it cancelled.
func (h *ExecutionControlHub) CancelRun(sessionID, runID string, cause *RunCancelledError) int {
	if h == nil || (sessionID == "" && runID == "") {
		return 0
	}
	h.mu.Lock()
	var hit []*trackedRun
	for t := range h.runs {
		if sessionID != "" && t.session != domain.SessionID(sessionID) {
			continue
		}
		if runID != "" && !t.hasRun(domain.RunID(runID)) {
			continue
		}
		hit = append(hit, t)
	}
	h.mu.Unlock()
	for _, t := range hit {
		t.cancel(cause)
	}
	return len(hit)
}
