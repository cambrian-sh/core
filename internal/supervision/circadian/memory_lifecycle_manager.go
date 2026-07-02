package circadian

import (
	"context"
	"log/slog"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// SessionConsolidator is the narrow interface MemoryLifecycleManager uses to
// run per-session memory consolidation (fulfilled by ConsolidatorAgent).
type SessionConsolidator interface {
	Consolidate(ctx context.Context, sess domain.Session) error
}

const defaultWorkerCount = 4

// MemoryLifecycleManager replaces CircadianRhythm. It orchestrates session
// lifecycle without assuming 24/7 uptime:
//   - On Start: drains dormant-session backlog via a bounded worker pool.
//   - At runtime: subscribes to SessionDormantEvent; schedules per-session
//     time.AfterFunc timers (no global ticker, no pg_cron dependency).
//
// ADR-0030.
type MemoryLifecycleManager struct {
	sessions     DormantSessionManager
	consolidator SessionConsolidator
	bus          domain.EventBus
	ttl          time.Duration
	workerCount  int
	backlog      chan domain.Session
}

// NewMemoryLifecycleManager constructs a ready-to-Start MemoryLifecycleManager.
//
//	sessions     – narrow session store interface (ListSessions, TransitionStatus)
//	consolidator – runs memory consolidation for a completed session
//	bus          – internal event bus; nil disables event publishing
//	ttl          – sessions older than ttl are treated as overdue on startup
func NewMemoryLifecycleManager(
	sessions DormantSessionManager,
	consolidator SessionConsolidator,
	bus domain.EventBus,
	ttl time.Duration,
) *MemoryLifecycleManager {
	return &MemoryLifecycleManager{
		sessions:     sessions,
		consolidator: consolidator,
		bus:          bus,
		ttl:          ttl,
		workerCount:  defaultWorkerCount,
		backlog:      make(chan domain.Session, 1024),
	}
}

// Start launches worker goroutines, enqueues the dormant-session backlog, and
// subscribes to runtime SessionDormantEvent — then returns immediately.
// The workers drain in the background; server readiness is not blocked.
func (m *MemoryLifecycleManager) Start(ctx context.Context) {
	for i := 0; i < m.workerCount; i++ {
		go m.workerLoop(ctx)
	}

	// Drain startup backlog asynchronously — do not block Start().
	go func() {
		sessions, err := m.sessions.ListSessions(ctx, domain.SessionDormant)
		if err != nil {
			slog.Warn("MemoryLifecycleManager: backlog query failed", "err", err)
			return
		}
		for _, sess := range sessions {
			select {
			case m.backlog <- sess:
			case <-ctx.Done():
				return
			}
		}
	}()

	if m.bus != nil {
		m.bus.Subscribe(domain.EventTypeSessionDormant, func(e domain.DomainEvent) {
			ev, ok := e.(domain.SessionDormantEvent)
			if !ok {
				return
			}
			m.scheduleSession(ctx, ev.SessionID, ev.DormantAt)
		})
		m.bus.Subscribe(domain.EventTypeMemoryPressure, func(_ domain.DomainEvent) {
			// Trigger global consolidation using an empty-ID session as the scope sentinel.
			go m.process(ctx, domain.Session{})
		})
	}
}

// workerLoop drains the backlog channel, classifying each session as overdue
// (process now) or not yet due (schedule for later).
func (m *MemoryLifecycleManager) workerLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case sess, ok := <-m.backlog:
			if !ok {
				return
			}
			if m.isOverdue(sess) {
				m.process(ctx, sess)
			} else {
				m.scheduleSession(ctx, sess.ID, sess.UpdatedAt)
			}
		}
	}
}

func (m *MemoryLifecycleManager) isOverdue(sess domain.Session) bool {
	if m.ttl <= 0 {
		return false
	}
	return time.Since(sess.UpdatedAt) >= m.ttl
}

// scheduleSession fires a one-shot timer that calls process after the
// remaining TTL elapses.
func (m *MemoryLifecycleManager) scheduleSession(ctx context.Context, sessionID string, dormantAt time.Time) {
	remaining := m.ttl - time.Since(dormantAt)
	if remaining <= 0 {
		// Already overdue — process directly on a goroutine so we don't block.
		go func() {
			sess := domain.Session{ID: sessionID, UpdatedAt: dormantAt}
			m.process(ctx, sess)
		}()
		return
	}

	time.AfterFunc(remaining, func() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		sess := domain.Session{ID: sessionID, UpdatedAt: dormantAt}
		m.process(ctx, sess)
	})
}

// process runs consolidation for a single session and publishes the completion event.
func (m *MemoryLifecycleManager) process(ctx context.Context, sess domain.Session) {
	if err := m.consolidator.Consolidate(ctx, sess); err != nil {
		slog.Warn("MemoryLifecycleManager: consolidation failed", "session", sess.ID, "err", err)
		return
	}
	_ = m.sessions.TransitionStatus(ctx, sess.ID, domain.SessionCompleted)
	if m.bus != nil {
		_ = m.bus.Publish(domain.SessionCompletedEvent{
			SessionID:      sess.ID,
			ConsolidatedAt: time.Now(),
		})
	}
}
