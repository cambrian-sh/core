package session_test

import (
	"context"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/substrate/session"
)

// captureEventBus records events published to it.
type captureEventBus struct {
	events []domain.DomainEvent
}

func (b *captureEventBus) Subscribe(_ string, _ domain.EventHandler) {}
func (b *captureEventBus) Publish(e domain.DomainEvent) error {
	b.events = append(b.events, e)
	return nil
}

// inmemSessionRepo is a minimal in-memory SessionRepository for tests.
type inmemSessionRepo struct {
	sessions map[string]domain.Session
}

func newInmem() *inmemSessionRepo { return &inmemSessionRepo{sessions: make(map[string]domain.Session)} }

func (r *inmemSessionRepo) SaveSession(_ context.Context, s domain.Session) error {
	r.sessions[s.ID] = s
	return nil
}
func (r *inmemSessionRepo) GetSession(_ context.Context, id string) (*domain.Session, error) {
	s, ok := r.sessions[id]
	if !ok {
		return nil, nil
	}
	return &s, nil
}
func (r *inmemSessionRepo) ListSessions(_ context.Context, status domain.SessionStatus) ([]domain.Session, error) {
	var out []domain.Session
	for _, s := range r.sessions {
		if status == "" || s.Status == status {
			out = append(out, s)
		}
	}
	return out, nil
}

// Cycle 1 — TransitionStatus to Dormant publishes SessionDormantEvent.
func TestSessionManager_TransitionToDormant_PublishesEvent(t *testing.T) {
	repo := newInmem()
	bus := &captureEventBus{}

	mgr := session.New(repo)
	mgr.SetEventBus(bus)

	ctx := context.Background()
	sess, _ := mgr.CreateSession(ctx, "test goal", "")

	if err := mgr.TransitionStatus(ctx, sess.ID, domain.SessionDormant); err != nil {
		t.Fatalf("TransitionStatus: %v", err)
	}

	if len(bus.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(bus.events))
	}
	ev, ok := bus.events[0].(domain.SessionDormantEvent)
	if !ok {
		t.Fatalf("expected SessionDormantEvent, got %T", bus.events[0])
	}
	if ev.SessionID != sess.ID {
		t.Errorf("expected SessionID %q, got %q", sess.ID, ev.SessionID)
	}
	if ev.DormantAt.IsZero() {
		t.Error("expected DormantAt to be set")
	}
}

// Cycle 2 — Transition to non-Dormant status does NOT publish SessionDormantEvent.
func TestSessionManager_TransitionToCompleted_DoesNotPublishDormantEvent(t *testing.T) {
	repo := newInmem()
	bus := &captureEventBus{}

	mgr := session.New(repo)
	mgr.SetEventBus(bus)

	ctx := context.Background()
	sess, _ := mgr.CreateSession(ctx, "goal", "")
	_ = mgr.TransitionStatus(ctx, sess.ID, domain.SessionDormant) // makes dormant first
	bus.events = nil                                               // reset

	_ = mgr.TransitionStatus(ctx, sess.ID, domain.SessionCompleted)

	for _, e := range bus.events {
		if _, isDormant := e.(domain.SessionDormantEvent); isDormant {
			t.Fatal("unexpected SessionDormantEvent on non-dormant transition")
		}
	}
}

// Cycle 3 — Nil EventBus is safe (no panic).
func TestSessionManager_NilEventBus_IsSafe(t *testing.T) {
	repo := newInmem()
	mgr := session.New(repo)
	// EventBus not set (nil)

	ctx := context.Background()
	sess, _ := mgr.CreateSession(ctx, "goal", "")
	if err := mgr.TransitionStatus(ctx, sess.ID, domain.SessionDormant); err != nil {
		t.Fatalf("TransitionStatus with nil bus: %v", err)
	}
}

// Cycle 4 — TTLDuration in the event matches the configured TTL.
func TestSessionManager_DormantEvent_IncludesTTL(t *testing.T) {
	repo := newInmem()
	bus := &captureEventBus{}
	ttl := 7 * 24 * time.Hour

	mgr := session.New(repo)
	mgr.SetEventBus(bus)
	mgr.SetTTL(ttl)

	ctx := context.Background()
	sess, _ := mgr.CreateSession(ctx, "goal", "")
	_ = mgr.TransitionStatus(ctx, sess.ID, domain.SessionDormant)

	ev := bus.events[0].(domain.SessionDormantEvent)
	if ev.TTLDuration != ttl {
		t.Errorf("expected TTL %v, got %v", ttl, ev.TTLDuration)
	}
}
