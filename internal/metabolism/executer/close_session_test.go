package executer

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

type fakeCloser struct {
	session    *domain.Session
	getErr     error
	closedTo   domain.SessionStatus
	closedWith string
	calls      int
}

func (f *fakeCloser) GetSession(context.Context, domain.SessionID) (*domain.Session, error) {
	return f.session, f.getErr
}

func (f *fakeCloser) TransitionStatusReason(_ context.Context, _ domain.SessionID, target domain.SessionStatus, reason string) error {
	f.calls++
	f.closedTo = target
	f.closedWith = reason
	return nil
}

func activeSession() *domain.Session {
	return &domain.Session{ID: "s1", Status: domain.SessionActive}
}

// The behaviour asked for: finished work closes its task. Nothing did this
// before — `completed` was only ever set by the operator's explicit command, so
// a task whose plan ran to the end stayed open indefinitely.
func TestCloseSession_ClosesAnActiveTask(t *testing.T) {
	c := &fakeCloser{session: activeSession()}
	d := &DAGExecutor{CurrentSessionID: "s1", SessionCloser: c}

	d.closeSession()

	if c.calls != 1 {
		t.Fatalf("want one transition, got %d", c.calls)
	}
	if c.closedTo != domain.SessionCompleted {
		t.Fatalf("want completed, got %q", c.closedTo)
	}
	if c.closedWith == "" {
		t.Fatal("the transition carries no reason; the audit trail needs one")
	}
}

// A PAUSED task is an operator decision. Closing it would overrule a human, and
// `completed` is irreversible — there is no undo.
func TestCloseSession_LeavesAPausedTaskAlone(t *testing.T) {
	c := &fakeCloser{session: &domain.Session{ID: "s1", Status: domain.SessionPaused}}
	d := &DAGExecutor{CurrentSessionID: "s1", SessionCloser: c}

	d.closeSession()

	if c.calls != 0 {
		t.Fatal("a paused task was closed out from under the operator")
	}
}

// Dormant may still wake on its next bid. Closing it ends a task the kernel
// intends to resume.
func TestCloseSession_LeavesADormantTaskAlone(t *testing.T) {
	c := &fakeCloser{session: &domain.Session{ID: "s1", Status: domain.SessionDormant}}
	d := &DAGExecutor{CurrentSessionID: "s1", SessionCloser: c}

	d.closeSession()

	if c.calls != 0 {
		t.Fatal("a dormant task was closed; it may still wake")
	}
}

// Already closed: the transition is idempotent, but asking for it again is
// noise. Reading first keeps an expected no-op out of the write path.
func TestCloseSession_DoesNotReCloseAFinishedTask(t *testing.T) {
	c := &fakeCloser{session: &domain.Session{ID: "s1", Status: domain.SessionCompleted}}
	d := &DAGExecutor{CurrentSessionID: "s1", SessionCloser: c}

	d.closeSession()

	if c.calls != 0 {
		t.Fatalf("re-closed an already finished task (%d calls)", c.calls)
	}
}

// A run with no session, or a kernel that wires no closer, must be a no-op
// rather than a panic — that is the previous behaviour and it stays correct.
func TestCloseSession_NoSessionOrNoCloserIsANoOp(t *testing.T) {
	(&DAGExecutor{CurrentSessionID: "s1"}).closeSession()

	c := &fakeCloser{session: activeSession()}
	(&DAGExecutor{SessionCloser: c}).closeSession()
	if c.calls != 0 {
		t.Fatal("closed a task for a run that belongs to none")
	}
}

// An unreadable session is left alone. Guessing here would close a task on the
// strength of a failed read, and the close cannot be undone.
func TestCloseSession_UnreadableSessionIsLeftOpen(t *testing.T) {
	c := &fakeCloser{getErr: errors.New("store down")}
	d := &DAGExecutor{CurrentSessionID: "s1", SessionCloser: c}

	d.closeSession()

	if c.calls != 0 {
		t.Fatal("closed a task whose status could not be read")
	}
}
