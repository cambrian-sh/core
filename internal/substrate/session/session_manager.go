package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// SessionRepository is the persistence interface for Sessions.
type SessionRepository interface {
	SaveSession(ctx context.Context, session domain.Session) error
	GetSession(ctx context.Context, id string) (*domain.Session, error)
	ListSessions(ctx context.Context, status domain.SessionStatus) ([]domain.Session, error)
}

// SessionManager manages the lifecycle of Sessions.
type SessionManager struct {
	store    SessionRepository
	eventBus domain.EventBus  // may be nil; publishes lifecycle events
	ttl      time.Duration    // published in SessionDormantEvent; 0 = unset
}

func New(store SessionRepository) *SessionManager {
	return &SessionManager{store: store}
}

// SetEventBus wires an EventBus so SessionManager publishes lifecycle events.
// Call before Start. ADR-0030.
func (m *SessionManager) SetEventBus(bus domain.EventBus) { m.eventBus = bus }

// SetTTL sets the TTL duration included in SessionDormantEvent. ADR-0030.
func (m *SessionManager) SetTTL(ttl time.Duration) { m.ttl = ttl }

// CreateSession creates a new Active session with a unique ID.
func (m *SessionManager) CreateSession(ctx context.Context, goal, parentID string) (*domain.Session, error) {
	now := time.Now()
	ses := domain.Session{
		ID:        newSessionID(),
		ParentID:  parentID,
		Goal:      goal,
		Status:    domain.SessionActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := m.store.SaveSession(ctx, ses); err != nil {
		return nil, err
	}
	return &ses, nil
}

// CreateScopedSession is CreateSession plus a non-forgeable caller_scope persisted
// server-side (ADR-0034 D13 Phase 2). The caller_scope is supplied by the
// integrating application at conversation start, NOT by the agent — and it is read
// back per-RPC from the session record, never from the forgeable Handoff.Context.
func (m *SessionManager) CreateScopedSession(ctx context.Context, goal, parentID string, caller domain.ScopeConfig) (*domain.Session, error) {
	now := time.Now()
	ses := domain.Session{
		ID:          newSessionID(),
		ParentID:    parentID,
		Goal:        goal,
		Status:      domain.SessionActive,
		CreatedAt:   now,
		UpdatedAt:   now,
		CallerScope: caller,
	}
	if err := m.store.SaveSession(ctx, ses); err != nil {
		return nil, err
	}
	return &ses, nil
}

// SetCallerScope persists (or updates) a session's caller_scope. ADR-0034 (D13).
func (m *SessionManager) SetCallerScope(ctx context.Context, sessionID string, caller domain.ScopeConfig) error {
	ses, err := m.store.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	ses.CallerScope = caller
	ses.UpdatedAt = time.Now()
	return m.store.SaveSession(ctx, *ses)
}

// CallerScope returns the persisted caller_scope for a session (zero/unrestricted
// when the session is unknown or carries none). It is the server-side, non-forgeable
// source of caller_scope for Phase-2 effective-scope re-derivation. ADR-0034 (D13).
func (m *SessionManager) CallerScope(ctx context.Context, sessionID string) domain.ScopeConfig {
	if sessionID == "" {
		return domain.ScopeConfig{}
	}
	ses, err := m.store.GetSession(ctx, sessionID)
	if err != nil || ses == nil {
		return domain.ScopeConfig{}
	}
	return ses.CallerScope
}

// GetSession retrieves a session by ID.
func (m *SessionManager) GetSession(ctx context.Context, id string) (*domain.Session, error) {
	return m.store.GetSession(ctx, id)
}

// ListSessions returns sessions filtered by status. Empty status returns all.
func (m *SessionManager) ListSessions(ctx context.Context, status domain.SessionStatus) ([]domain.Session, error) {
	return m.store.ListSessions(ctx, status)
}

// TransitionStatus moves a session to the target status. Returns an error
// if the transition is invalid according to the state machine.
func (m *SessionManager) TransitionStatus(ctx context.Context, sessionID string, target domain.SessionStatus) error {
	ses, err := m.store.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	if ses == nil {
		return fmt.Errorf("session %s not found", sessionID)
	}

	if ses.Status == target {
		return nil // idempotent
	}

	if !isValidTransition(ses.Status, target) {
		return fmt.Errorf("invalid session transition: %s → %s", ses.Status, target)
	}

	now := time.Now()
	ses.Status = target
	ses.UpdatedAt = now
	if target == domain.SessionCompleted {
		ses.CompletedAt = now
	}
	if err := m.store.SaveSession(ctx, *ses); err != nil {
		return err
	}
	if target == domain.SessionDormant && m.eventBus != nil {
		_ = m.eventBus.Publish(domain.SessionDormantEvent{
			SessionID:   ses.ID,
			DormantAt:   now,
			TTLDuration: m.ttl,
		})
	}
	return nil
}

func isValidTransition(current, target domain.SessionStatus) bool {
	switch current {
	case domain.SessionActive:
		return target == domain.SessionPaused || target == domain.SessionDormant || target == domain.SessionCompleted
	case domain.SessionPaused:
		return target == domain.SessionActive || target == domain.SessionDormant
	case domain.SessionDormant:
		return target == domain.SessionActive || target == domain.SessionCompleted
	case domain.SessionCompleted:
		return false
	default:
		return false
	}
}

func newSessionID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
