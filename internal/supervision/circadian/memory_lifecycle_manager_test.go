package circadian_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
	"github.com/cambrian-sh/cambrian-runtime/internal/supervision/circadian"
)

// ── Test doubles ─────────────────────────────────────────────────────────────

type stubSessionStore struct {
	mu       sync.Mutex
	sessions []domain.Session
}

func (s *stubSessionStore) ListSessions(_ context.Context, status domain.SessionStatus) ([]domain.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.Session
	for _, sess := range s.sessions {
		if status == "" || sess.Status == status {
			out = append(out, sess)
		}
	}
	return out, nil
}

func (s *stubSessionStore) TransitionStatus(_ context.Context, _ string, _ domain.SessionStatus) error {
	return nil
}

type countingConsolidator struct {
	mu    sync.Mutex
	calls []string
}

func (c *countingConsolidator) Consolidate(_ context.Context, sess domain.Session) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, sess.ID)
	return nil
}

func (c *countingConsolidator) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

type stubBus struct {
	published []domain.DomainEvent
	mu        sync.Mutex
	handlers  map[string][]domain.EventHandler
}

func newStubBus() *stubBus { return &stubBus{handlers: make(map[string][]domain.EventHandler)} }

func (b *stubBus) Subscribe(eventType string, h domain.EventHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[eventType] = append(b.handlers[eventType], h)
}
func (b *stubBus) Publish(e domain.DomainEvent) error {
	b.mu.Lock()
	b.published = append(b.published, e)
	handlers := make([]domain.EventHandler, len(b.handlers[e.EventType()]))
	copy(handlers, b.handlers[e.EventType()])
	b.mu.Unlock()
	for _, h := range handlers {
		h(e)
	}
	return nil
}

// ── Tests ────────────────────────────────────────────────────────────────────

// Cycle 1 — Start() returns immediately even with many dormant sessions.
func TestMLM_Start_ReturnsImmediately(t *testing.T) {
	sessions := make([]domain.Session, 20)
	for i := range sessions {
		sessions[i] = domain.Session{
			ID:        "sess-" + string(rune('A'+i)),
			Status:    domain.SessionDormant,
			UpdatedAt: time.Now().Add(-10 * 24 * time.Hour), // 10 days old
		}
	}
	store := &stubSessionStore{sessions: sessions}
	cons := &countingConsolidator{}
	bus := newStubBus()

	mlm := circadian.NewMemoryLifecycleManager(store, cons, bus, 1*24*time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		mlm.Start(ctx)
		close(done)
	}()

	select {
	case <-done:
		// Start() returned — good
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Start() blocked for more than 500ms — should return immediately")
	}
}

// Cycle 2 — Overdue dormant sessions (past TTL) are consolidated by the worker pool.
func TestMLM_Start_ProcessesOverdueSessions(t *testing.T) {
	sessions := []domain.Session{
		{ID: "overdue-1", Status: domain.SessionDormant, UpdatedAt: time.Now().Add(-10 * 24 * time.Hour)},
		{ID: "overdue-2", Status: domain.SessionDormant, UpdatedAt: time.Now().Add(-8 * 24 * time.Hour)},
	}
	store := &stubSessionStore{sessions: sessions}
	cons := &countingConsolidator{}
	bus := newStubBus()

	mlm := circadian.NewMemoryLifecycleManager(store, cons, bus, 7*24*time.Hour) // TTL = 7 days

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mlm.Start(ctx)

	// Workers drain asynchronously — give them time.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && cons.count() < 2 {
		time.Sleep(10 * time.Millisecond)
	}

	if got := cons.count(); got != 2 {
		t.Errorf("expected 2 overdue sessions consolidated, got %d", got)
	}
}

// Cycle 3 — Non-overdue sessions are NOT processed immediately.
func TestMLM_Start_DoesNotProcessFutureSessions(t *testing.T) {
	sessions := []domain.Session{
		{ID: "not-overdue", Status: domain.SessionDormant, UpdatedAt: time.Now().Add(-1 * time.Hour)},
	}
	store := &stubSessionStore{sessions: sessions}
	cons := &countingConsolidator{}
	bus := newStubBus()

	mlm := circadian.NewMemoryLifecycleManager(store, cons, bus, 7*24*time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mlm.Start(ctx)

	// Wait a bit — non-overdue sessions should NOT be consolidated yet.
	time.Sleep(50 * time.Millisecond)

	if cons.count() != 0 {
		t.Errorf("expected 0 consolidations for non-overdue session, got %d", cons.count())
	}
}

// Cycle 4 — A runtime SessionDormantEvent triggers a per-session timer.
func TestMLM_OnSessionDormant_SchedulesFutureConsolidation(t *testing.T) {
	store := &stubSessionStore{}
	cons := &countingConsolidator{}
	bus := newStubBus()

	mlm := circadian.NewMemoryLifecycleManager(store, cons, bus, 100*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mlm.Start(ctx)

	_ = bus.Publish(domain.SessionDormantEvent{
		SessionID:   "runtime-session",
		DormantAt:   time.Now(),
		TTLDuration: 100 * time.Millisecond,
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && cons.count() < 1 {
		time.Sleep(10 * time.Millisecond)
	}

	if cons.count() < 1 {
		t.Error("expected consolidation to run after TTL, but it did not")
	}
}

// Cycle 5 — SessionCompletedEvent is published after consolidation.
func TestMLM_PublishesSessionCompletedEvent(t *testing.T) {
	store := &stubSessionStore{}
	cons := &countingConsolidator{}
	bus := newStubBus()

	var completedCount int32
	bus.Subscribe(domain.EventTypeSessionCompleted, func(e domain.DomainEvent) {
		atomic.AddInt32(&completedCount, 1)
	})

	mlm := circadian.NewMemoryLifecycleManager(store, cons, bus, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mlm.Start(ctx)

	_ = bus.Publish(domain.SessionDormantEvent{
		SessionID:   "sess-x",
		DormantAt:   time.Now(),
		TTLDuration: 10 * time.Millisecond,
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&completedCount) < 1 {
		time.Sleep(10 * time.Millisecond)
	}

	if atomic.LoadInt32(&completedCount) == 0 {
		t.Error("expected SessionCompletedEvent to be published after consolidation")
	}
}
