package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ErrConversationIndexUnsupported is returned when the wired store cannot answer "which
// sessions did this conversation start?". Explicit rather than an empty result, so a caller
// can tell "none" from "this backend does not index that".
var ErrConversationIndexUnsupported = errors.New("session store does not index sessions by conversation")

// ConversationSessionLister is the optional half of SessionRepository that answers the
// ADR-0084 D2 question. Implemented by the Postgres store (it has the index for it).
type ConversationSessionLister interface {
	ListSessionsForConversation(ctx context.Context, conversationID string) ([]domain.Session, error)
}

// SessionRepository is the persistence interface for Sessions.
type SessionRepository interface {
	SaveSession(ctx context.Context, session domain.Session) error
	GetSession(ctx context.Context, id domain.SessionID) (*domain.Session, error)
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
func (m *SessionManager) CreateSession(ctx context.Context, goal string, parentID domain.SessionID) (*domain.Session, error) {
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
	m.publishState(ses, "")
	return &ses, nil
}

// CreateScopedSession is CreateSession plus a non-forgeable caller_scope persisted
// server-side (ADR-0034 D13 Phase 2). The caller_scope is supplied by the
// integrating application at conversation start, NOT by the agent — and it is read
// back per-RPC from the session record, never from the forgeable Handoff.Context.
func (m *SessionManager) CreateScopedSession(ctx context.Context, goal string, parentID domain.SessionID, caller domain.ScopeConfig) (*domain.Session, error) {
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
	m.publishState(ses, "")
	return &ses, nil
}

// SetCallerScope persists (or updates) a session's caller_scope. ADR-0034 (D13).
func (m *SessionManager) SetCallerScope(ctx context.Context, sessionID domain.SessionID, caller domain.ScopeConfig) error {
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
func (m *SessionManager) CallerScope(ctx context.Context, sessionID domain.SessionID) domain.ScopeConfig {
	if sessionID == "" {
		return domain.ScopeConfig{}
	}
	ses, err := m.store.GetSession(ctx, sessionID)
	if err != nil || ses == nil {
		return domain.ScopeConfig{}
	}
	return ses.CallerScope
}

// SaveConversationLink records which conversation turn ordered this session's work
// (ADR-0084 D2).
//
// Write-once: an already-linked session keeps its original origin. A session belongs to the
// turn that STARTED it, and later turns that happen to touch it must not rewrite that — the
// link is causation, and causation does not change retroactively.
func (m *SessionManager) SaveConversationLink(ctx context.Context, sessionID domain.SessionID, conversationID, originMessageID string) error {
	if conversationID == "" {
		return nil
	}
	ses, err := m.store.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	if ses == nil {
		return fmt.Errorf("session %s not found", sessionID)
	}
	if ses.ConversationID != "" {
		return nil // already attributed
	}
	ses.ConversationID = conversationID
	ses.OriginMessageID = originMessageID
	ses.UpdatedAt = time.Now()
	if err := m.store.SaveSession(ctx, *ses); err != nil {
		return err
	}
	m.publishState(*ses, "")
	return nil
}

// ListSessionsForConversation returns the sessions a conversation set in motion, if the
// store can answer it. Not every backend indexes the link; a store that cannot returns
// ErrConversationIndexUnsupported rather than silently scanning everything.
func (m *SessionManager) ListSessionsForConversation(ctx context.Context, conversationID string) ([]domain.Session, error) {
	lister, ok := m.store.(ConversationSessionLister)
	if !ok {
		return nil, ErrConversationIndexUnsupported
	}
	return lister.ListSessionsForConversation(ctx, conversationID)
}

// GetSession retrieves a session by ID.
func (m *SessionManager) GetSession(ctx context.Context, id domain.SessionID) (*domain.Session, error) {
	return m.store.GetSession(ctx, id)
}

// ListSessions returns sessions filtered by status. Empty status returns all.
func (m *SessionManager) ListSessions(ctx context.Context, status domain.SessionStatus) ([]domain.Session, error) {
	return m.store.ListSessions(ctx, status)
}

// TransitionStatus moves a session to the target status. Returns an error
// if the transition is invalid according to the state machine.
func (m *SessionManager) TransitionStatus(ctx context.Context, sessionID domain.SessionID, target domain.SessionStatus) error {
	return m.TransitionStatusReason(ctx, sessionID, target, "")
}

// TransitionStatusReason is TransitionStatus carrying the operator's justification, which
// travels on the emitted SessionStateEvent so the console can show WHY a session changed
// state rather than just that it did. Kernel-driven transitions pass "".
func (m *SessionManager) TransitionStatusReason(ctx context.Context, sessionID domain.SessionID, target domain.SessionStatus, reason string) error {
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
	// Absolute state first: this is the event that covers ALL five transitions. The
	// dormant-specific event below is kept for consumers that predate it.
	m.publishState(*ses, reason)
	if target == domain.SessionDormant && m.eventBus != nil {
		_ = m.eventBus.Publish(domain.SessionDormantEvent{
			SessionID:   ses.ID,
			DormantAt:   now,
			TTLDuration: m.ttl,
		})
	}
	return nil
}

// publishState emits the absolute lifecycle state of a session. Best-effort: a nil bus
// (or a publish error) must never fail the transition that already committed to the store.
func (m *SessionManager) publishState(ses domain.Session, reason string) {
	if m.eventBus == nil {
		return
	}
	_ = m.eventBus.Publish(domain.SessionStateEvent{
		SessionID: ses.ID,
		Status:    ses.Status,
		Goal:      ses.Goal,
		ParentID:  ses.ParentID,
		CreatedAt: ses.CreatedAt,
		UpdatedAt: ses.UpdatedAt,
		Reason:    reason,
	})
}

// SweepIdle transitions ACTIVE sessions untouched for longer than idleFor to DORMANT,
// returning how many it moved.
//
// This is the missing driver of the lifecycle. Consolidation (ADR-0012/0030) subscribes to
// the dormant transition, but nothing ever performed one: sessions were created active and
// stayed active forever, so the consolidation pipeline was unreachable and the session store
// grew without bound. A non-positive idleFor disables the sweep.
func (m *SessionManager) SweepIdle(ctx context.Context, idleFor time.Duration) (int, error) {
	if idleFor <= 0 {
		return 0, nil
	}
	active, err := m.store.ListSessions(ctx, domain.SessionActive)
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-idleFor)
	moved := 0
	for _, ses := range active {
		if ses.UpdatedAt.After(cutoff) {
			continue
		}
		if err := m.TransitionStatus(ctx, ses.ID, domain.SessionDormant); err != nil {
			continue // a concurrent transition won; not this sweep's problem
		}
		moved++
	}
	return moved, nil
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

func newSessionID() domain.SessionID {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return domain.SessionID(hex.EncodeToString(b))
}
