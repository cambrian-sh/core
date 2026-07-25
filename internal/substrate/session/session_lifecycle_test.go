package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// recordingBus captures published events for assertion.
type recordingBus struct {
	mu     sync.Mutex
	events []domain.DomainEvent
}

func (b *recordingBus) Publish(e domain.DomainEvent) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, e)
	return nil
}
func (b *recordingBus) Subscribe(string, domain.EventHandler) {}

func (b *recordingBus) states() []domain.SessionStateEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []domain.SessionStateEvent
	for _, e := range b.events {
		if se, ok := e.(domain.SessionStateEvent); ok {
			out = append(out, se)
		}
	}
	return out
}

func newLifecycleMgr(t *testing.T) (*SessionManager, *stubSessionStore, *recordingBus) {
	t.Helper()
	store := newStubSessionStore()
	bus := &recordingBus{}
	m := New(store)
	m.SetEventBus(bus)
	return m, store, bus
}

// Creation must be observable. Without this the operator feed carries no "a session now
// exists" signal at all, so a console only learns about new sessions on the next snapshot.
func TestCreateSession_PublishesState(t *testing.T) {
	m, _, bus := newLifecycleMgr(t)

	ses, err := m.CreateSession(context.Background(), "ship the thing", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	states := bus.states()
	if len(states) != 1 {
		t.Fatalf("expected 1 state event on create, got %d", len(states))
	}
	if states[0].SessionID != ses.ID {
		t.Errorf("session id = %q, want %q", states[0].SessionID, ses.ID)
	}
	if states[0].Status != domain.SessionActive {
		t.Errorf("status = %q, want active", states[0].Status)
	}
	if states[0].Goal != "ship the thing" {
		t.Errorf("goal = %q", states[0].Goal)
	}
	if states[0].CreatedAt.IsZero() {
		t.Error("CreatedAt must be set — the console renders it and shows \"—\" when empty")
	}
}

// Every transition emits absolute state, including the two the old per-transition events
// never covered (paused, and the resume back to active).
func TestTransition_PublishesStateForEveryTransition(t *testing.T) {
	m, _, bus := newLifecycleMgr(t)
	ctx := context.Background()
	ses, _ := m.CreateSession(ctx, "goal", "")

	for _, target := range []domain.SessionStatus{
		domain.SessionPaused, domain.SessionActive, domain.SessionDormant, domain.SessionCompleted,
	} {
		if err := m.TransitionStatus(ctx, ses.ID, target); err != nil {
			t.Fatalf("transition to %s: %v", target, err)
		}
	}

	states := bus.states()
	// 1 create + 4 transitions
	if len(states) != 5 {
		t.Fatalf("expected 5 state events, got %d", len(states))
	}
	want := []domain.SessionStatus{
		domain.SessionActive, domain.SessionPaused, domain.SessionActive,
		domain.SessionDormant, domain.SessionCompleted,
	}
	for i, w := range want {
		if states[i].Status != w {
			t.Errorf("state[%d] = %q, want %q", i, states[i].Status, w)
		}
	}
}

// An operator's justification travels with the state change, so the console can show WHY a
// session moved rather than only that it did.
func TestTransitionStatusReason_CarriesReason(t *testing.T) {
	m, _, bus := newLifecycleMgr(t)
	ctx := context.Background()
	ses, _ := m.CreateSession(ctx, "goal", "")

	if err := m.TransitionStatusReason(ctx, ses.ID, domain.SessionPaused, "customer escalation"); err != nil {
		t.Fatalf("transition: %v", err)
	}
	states := bus.states()
	last := states[len(states)-1]
	if last.Reason != "customer escalation" {
		t.Errorf("reason = %q, want %q", last.Reason, "customer escalation")
	}
	// Kernel-driven transitions carry no reason.
	if states[0].Reason != "" {
		t.Errorf("creation should carry no reason, got %q", states[0].Reason)
	}
}

// The driver the lifecycle never had: idle ACTIVE sessions become DORMANT.
func TestSweepIdle_AgesIdleActiveSessions(t *testing.T) {
	m, store, _ := newLifecycleMgr(t)
	ctx := context.Background()

	stale, _ := m.CreateSession(ctx, "stale", "")
	fresh, _ := m.CreateSession(ctx, "fresh", "")

	// Backdate the stale one past the idle window.
	s := store.sessions[stale.ID]
	s.UpdatedAt = time.Now().Add(-2 * time.Hour)
	store.sessions[stale.ID] = s

	moved, err := m.SweepIdle(ctx, time.Hour)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if moved != 1 {
		t.Fatalf("expected 1 session aged, got %d", moved)
	}
	if got := store.sessions[stale.ID].Status; got != domain.SessionDormant {
		t.Errorf("stale session status = %q, want dormant", got)
	}
	if got := store.sessions[fresh.ID].Status; got != domain.SessionActive {
		t.Errorf("fresh session must stay active, got %q", got)
	}
}

// A non-positive window disables the sweep — no session is aged by accident.
func TestSweepIdle_DisabledAtZero(t *testing.T) {
	m, store, _ := newLifecycleMgr(t)
	ctx := context.Background()
	ses, _ := m.CreateSession(ctx, "goal", "")
	s := store.sessions[ses.ID]
	s.UpdatedAt = time.Now().Add(-100 * time.Hour)
	store.sessions[ses.ID] = s

	moved, err := m.SweepIdle(ctx, 0)
	if err != nil || moved != 0 {
		t.Fatalf("sweep with zero window: moved=%d err=%v", moved, err)
	}
	if got := store.sessions[ses.ID].Status; got != domain.SessionActive {
		t.Errorf("status = %q, want active (sweep disabled)", got)
	}
}

// The sweep only touches ACTIVE sessions — a paused session is paused deliberately by an
// operator and must not be aged out from under them.
func TestSweepIdle_LeavesPausedSessionsAlone(t *testing.T) {
	m, store, _ := newLifecycleMgr(t)
	ctx := context.Background()
	ses, _ := m.CreateSession(ctx, "goal", "")
	if err := m.TransitionStatus(ctx, ses.ID, domain.SessionPaused); err != nil {
		t.Fatalf("pause: %v", err)
	}
	s := store.sessions[ses.ID]
	s.UpdatedAt = time.Now().Add(-100 * time.Hour)
	store.sessions[ses.ID] = s

	moved, _ := m.SweepIdle(ctx, time.Hour)
	if moved != 0 {
		t.Errorf("paused sessions must not be aged, moved=%d", moved)
	}
	if got := store.sessions[ses.ID].Status; got != domain.SessionPaused {
		t.Errorf("status = %q, want paused", got)
	}
}

// A completed session is terminal: the sweep must not resurrect or re-transition it.
func TestSweepIdle_IgnoresCompleted(t *testing.T) {
	m, store, _ := newLifecycleMgr(t)
	ctx := context.Background()
	ses, _ := m.CreateSession(ctx, "goal", "")
	_ = m.TransitionStatus(ctx, ses.ID, domain.SessionCompleted)
	s := store.sessions[ses.ID]
	s.UpdatedAt = time.Now().Add(-100 * time.Hour)
	store.sessions[ses.ID] = s

	moved, _ := m.SweepIdle(ctx, time.Hour)
	if moved != 0 {
		t.Errorf("completed sessions must be terminal, moved=%d", moved)
	}
}

// A nil bus must never break a transition that already committed to the store.
func TestPublishState_NilBusIsSafe(t *testing.T) {
	m := New(newStubSessionStore())
	ctx := context.Background()
	ses, err := m.CreateSession(ctx, "goal", "")
	if err != nil {
		t.Fatalf("create with nil bus: %v", err)
	}
	if err := m.TransitionStatus(ctx, ses.ID, domain.SessionPaused); err != nil {
		t.Fatalf("transition with nil bus: %v", err)
	}
}
