package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/infrastructure/mcpserve"
	"github.com/cambrian-sh/core/internal/substrate/network"
	session "github.com/cambrian-sh/core/internal/substrate/session"
)

// The MCP endpoint's task lane (ADR-0126 D10, phase 4).
//
// This is deliberately NOT a task engine. Submission takes exactly the path an
// operator-submitted task takes — CreateScopedSession on the one session
// manager, then the one Execute entrypoint with the session presented as
// x-session-id — so plan generation, session binding, run persistence,
// checkpointing, steering and the operator feed are all the existing code
// (mirroring app.go's SessionOps SendFn and the contract-0074 planSubmitter).
//
// What the lane OWNS is the memory-resident task index: task id → beneficiary
// owner principal + outcome. Owner ruling (2026-08-21): the beneficiary rides
// an in-memory index keyed by the task id, not a session-store column — the
// session record is lease-scoped plumbing, and the task's live state (the
// Execute goroutine, its lease bindings, the fleet's held polls) is
// memory-resident already, so the index shares its residence and its lifetime.
// The consequence is honest and reported: task visibility and status do not
// survive a kernel restart, exactly as the running execution does not.
//
// The index is the production seeder of domain.WithTaskBeneficiary: the
// network plane resolves a caller's lease → task session → this index →
// beneficiary, and menu building attaches that owner's live worker fleet
// (ADR-0127 D4). Seeded ONLY here, from the authenticated MCP caller — never
// from a request payload (INV-5).

// taskResultMaxChars caps the result text one task retains. The full result
// still went wherever the execution path put it (artifacts, the feed); this is
// the polling answer, not an archive.
const taskResultMaxChars = 20_000

// taskFailurePrefix opens every failure note, so a failed task's result is
// recognizably a failure narrative rather than a produced answer.
const taskFailurePrefix = "task failed: "

// mcpTaskExecute runs one submitted task to completion through the kernel's
// execute lane. Synchronous — the lane calls it from the dispatch goroutine.
type mcpTaskExecute func(ctx context.Context, sessionID, task string) (result string, meta map[string]string, err error)

// mcpTaskLane implements mcpserve.TaskLane over the kernel's existing task
// machinery, and network.TaskBeneficiarySource for the menu-build seeding.
type mcpTaskLane struct {
	sessions *session.SessionManager
	authz    domain.Authorizer
	execute  mcpTaskExecute

	mu    sync.RWMutex
	tasks map[domain.SessionID]*mcpTaskRecord
}

type mcpTaskRecord struct {
	goal        string
	beneficiary domain.PrincipalRef
	state       string
	result      string
	createdAt   time.Time
	updatedAt   time.Time
}

var (
	_ mcpserve.TaskLane             = (*mcpTaskLane)(nil)
	_ network.TaskBeneficiarySource = (*mcpTaskLane)(nil)
)

// newMCPTaskLane builds the lane. sessions and execute are required for
// Submit to accept anything; a lane missing either refuses submissions with an
// honest error rather than half-accepting them.
func newMCPTaskLane(sessions *session.SessionManager, authz domain.Authorizer, execute mcpTaskExecute) *mcpTaskLane {
	return &mcpTaskLane{
		sessions: sessions,
		authz:    authz,
		execute:  execute,
		tasks:    map[domain.SessionID]*mcpTaskRecord{},
	}
}

// Submit opens the task session and dispatches asynchronously, exactly as the
// operator lane does. The task id IS the session id — the identifier the
// kernel already mints (crypto/rand) and already hands to operator clients.
func (l *mcpTaskLane) Submit(ctx context.Context, task, parentID string, beneficiary domain.PrincipalRef) (string, error) {
	if l.sessions == nil || l.execute == nil {
		return "", errors.New("the task lane is not wired on this kernel")
	}
	if beneficiary.IsZero() {
		// The transport derives the beneficiary before calling here; a zero one
		// reaching this point is a wiring defect, and a task nobody is the
		// beneficiary of must not exist (it would sit exactly where a matching
		// zero context could reach it).
		return "", errors.New("task lane: no beneficiary principal established for this caller")
	}
	// BRAIN-01, applied to this surface too: the opener's scope term is
	// resolved server-side from the AUTHENTICATED principal and persisted on
	// the session, so per-session read narrowing works for external submitters
	// exactly as it does for operators.
	scope := callerScopeForRef(ctx, l.authz, domain.PrincipalFromContext(ctx), domain.SurfaceFromContext(ctx))
	ses, err := l.sessions.CreateScopedSession(ctx, task, domain.SessionID(parentID), scope)
	if err != nil {
		return "", fmt.Errorf("open task session: %w", err)
	}
	now := time.Now().UTC()
	l.mu.Lock()
	l.tasks[ses.ID] = &mcpTaskRecord{
		goal:        task,
		beneficiary: beneficiary,
		state:       mcpserve.TaskStateRunning,
		createdAt:   now,
		updatedAt:   now,
	}
	l.mu.Unlock()
	go l.run(ses.ID, task)
	return string(ses.ID), nil
}

// run drives one task through Execute and records the terminal outcome. It is
// the async half the operator lane fires and forgets (SendFn); here the
// returned result is the whole point of the polling contract, so it is kept.
func (l *mcpTaskLane) run(id domain.SessionID, task string) {
	// Detached on purpose: the submitting request is long gone by the time the
	// plan finishes, and its cancellation must not be what kills the task.
	result, meta, err := l.execute(context.Background(), string(id), task)

	state := mcpserve.TaskStateSucceeded
	note := truncateTaskText(result)
	switch {
	case err != nil:
		// Classified, not forwarded: the note reaches an external caller, and
		// chatFailureNote exists precisely to say what went wrong without
		// leaking provider ids or internal error strings. The log has the rest.
		state, note = mcpserve.TaskStateFailed, taskFailurePrefix+chatFailureNote(err)
	case meta["_partial_plan"] == "true":
		// Execute answers a partial plan as a payload, not an error (the
		// operator watches the feed); for a polling caller it is a failure
		// with the partial-plan narrative as the detail.
		state, note = mcpserve.TaskStateFailed, taskFailurePrefix+truncateTaskText(result)
	}

	l.mu.Lock()
	if t := l.tasks[id]; t != nil {
		t.state, t.result, t.updatedAt = state, note, time.Now().UTC()
	}
	l.mu.Unlock()
}

// Status reads one task from the index, refined by the session's live status
// while the task is still running — pause/resume are operator steering on the
// SAME session, and the polling caller should see them.
func (l *mcpTaskLane) Status(ctx context.Context, taskID string) (mcpserve.TaskView, bool) {
	id := domain.SessionID(taskID)
	l.mu.RLock()
	t := l.tasks[id]
	var view mcpserve.TaskView
	if t != nil {
		view = mcpserve.TaskView{
			ID:          taskID,
			Goal:        t.goal,
			State:       t.state,
			Result:      t.result,
			Beneficiary: t.beneficiary,
			CreatedAt:   t.createdAt,
			UpdatedAt:   t.updatedAt,
		}
	}
	l.mu.RUnlock()
	if t == nil {
		return mcpserve.TaskView{}, false
	}
	if view.State == mcpserve.TaskStateRunning && l.sessions != nil {
		if ses, err := l.sessions.GetSession(ctx, id); err == nil && ses != nil {
			switch ses.Status {
			case domain.SessionPaused:
				view.State = mcpserve.TaskStatePaused
			case domain.SessionDormant:
				view.State = mcpserve.TaskStateDormant
			case domain.SessionCompleted:
				// The executor closed the session an instant before the
				// dispatch goroutine recorded the outcome; succeeded is
				// already true, the result lands on the next poll.
				view.State = mcpserve.TaskStateSucceeded
			}
		}
	}
	return view, true
}

// BeneficiaryOf is the network plane's read (network.TaskBeneficiarySource):
// lease → task session → the owner principal whose live fleet may serve this
// task. Zero for anything this lane did not submit — fail-closed, which is
// exactly the pre-phase-4 behaviour for every other session.
func (l *mcpTaskLane) BeneficiaryOf(task domain.SessionID) domain.PrincipalRef {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if t := l.tasks[task]; t != nil {
		return t.beneficiary
	}
	return domain.PrincipalRef{}
}

// truncateTaskText caps a stored result, marking the cut.
func truncateTaskText(s string) string {
	runes := []rune(s)
	if len(runes) <= taskResultMaxChars {
		return s
	}
	return string(runes[:taskResultMaxChars]) + "… [truncated]"
}
