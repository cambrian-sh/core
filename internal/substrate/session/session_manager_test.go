package session

import (
	"context"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

type stubSessionStore struct {
	sessions map[string]domain.Session
}

func newStubSessionStore() *stubSessionStore {
	return &stubSessionStore{sessions: make(map[string]domain.Session)}
}

func (s *stubSessionStore) SaveSession(_ context.Context, ses domain.Session) error {
	s.sessions[ses.ID] = ses
	return nil
}

func (s *stubSessionStore) GetSession(_ context.Context, id string) (*domain.Session, error) {
	ses, ok := s.sessions[id]
	if !ok {
		return nil, nil
	}
	return &ses, nil
}

func (s *stubSessionStore) ListSessions(_ context.Context, status domain.SessionStatus) ([]domain.Session, error) {
	var out []domain.Session
	for _, ses := range s.sessions {
		if status == "" || ses.Status == status {
			out = append(out, ses)
		}
	}
	return out, nil
}

func TestSessionManager_CreateSession(t *testing.T) {
	store := newStubSessionStore()
	mgr := New(store)

	ctx := context.Background()
	ses, err := mgr.CreateSession(ctx, "Build HTTP bridge", "")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if ses.ID == "" {
		t.Fatal("expected non-empty ID")
	}
	if ses.Goal != "Build HTTP bridge" {
		t.Errorf("Goal: got %q, want %q", ses.Goal, "Build HTTP bridge")
	}
	if ses.Status != domain.SessionActive {
		t.Errorf("Status: got %q, want %q", ses.Status, domain.SessionActive)
	}
	if ses.ParentID != "" {
		t.Errorf("ParentID: got %q, want empty", ses.ParentID)
	}
	if ses.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
}

func TestSessionManager_CreateSession_WithParent(t *testing.T) {
	store := newStubSessionStore()
	mgr := New(store)

	ctx := context.Background()
	ses, err := mgr.CreateSession(ctx, "Branch attempt", "parent-sess")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if ses.ParentID != "parent-sess" {
		t.Errorf("ParentID: got %q, want %q", ses.ParentID, "parent-sess")
	}
}

func TestSessionManager_GetSession(t *testing.T) {
	store := newStubSessionStore()
	mgr := New(store)

	ctx := context.Background()
	created, _ := mgr.CreateSession(ctx, "Test goal", "")

	got, err := mgr.GetSession(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got == nil {
		t.Fatal("GetSession returned nil")
	}
	if got.ID != created.ID {
		t.Errorf("ID mismatch: got %q, want %q", got.ID, created.ID)
	}
}

func TestSessionManager_Transition_ActiveToPaused(t *testing.T) {
	store := newStubSessionStore()
	mgr := New(store)

	ctx := context.Background()
	ses, _ := mgr.CreateSession(ctx, "test", "")

	err := mgr.TransitionStatus(ctx, ses.ID, domain.SessionPaused)
	if err != nil {
		t.Fatalf("TransitionStatus Active→Paused: %v", err)
	}
	updated, _ := mgr.GetSession(ctx, ses.ID)
	if updated.Status != domain.SessionPaused {
		t.Errorf("expected Paused, got %q", updated.Status)
	}
}

func TestSessionManager_Transition_ActiveToDormant(t *testing.T) {
	store := newStubSessionStore()
	mgr := New(store)

	ctx := context.Background()
	ses, _ := mgr.CreateSession(ctx, "test", "")

	err := mgr.TransitionStatus(ctx, ses.ID, domain.SessionDormant)
	if err != nil {
		t.Fatalf("TransitionStatus Active→Dormant: %v", err)
	}
}

func TestSessionManager_Transition_ActiveToCompleted(t *testing.T) {
	store := newStubSessionStore()
	mgr := New(store)

	ctx := context.Background()
	ses, _ := mgr.CreateSession(ctx, "test", "")

	err := mgr.TransitionStatus(ctx, ses.ID, domain.SessionCompleted)
	if err != nil {
		t.Fatalf("TransitionStatus Active→Completed: %v", err)
	}
}

func TestSessionManager_Transition_PausedToActive(t *testing.T) {
	store := newStubSessionStore()
	mgr := New(store)

	ctx := context.Background()
	ses, _ := mgr.CreateSession(ctx, "test", "")
	_ = mgr.TransitionStatus(ctx, ses.ID, domain.SessionPaused)

	err := mgr.TransitionStatus(ctx, ses.ID, domain.SessionActive)
	if err != nil {
		t.Fatalf("TransitionStatus Paused→Active: %v", err)
	}
}

func TestSessionManager_Transition_PausedToDormant(t *testing.T) {
	store := newStubSessionStore()
	mgr := New(store)

	ctx := context.Background()
	ses, _ := mgr.CreateSession(ctx, "test", "")
	_ = mgr.TransitionStatus(ctx, ses.ID, domain.SessionPaused)

	err := mgr.TransitionStatus(ctx, ses.ID, domain.SessionDormant)
	if err != nil {
		t.Fatalf("TransitionStatus Paused→Dormant: %v", err)
	}
}

func TestSessionManager_Transition_DormantToActive(t *testing.T) {
	store := newStubSessionStore()
	mgr := New(store)

	ctx := context.Background()
	ses, _ := mgr.CreateSession(ctx, "test", "")
	_ = mgr.TransitionStatus(ctx, ses.ID, domain.SessionDormant)

	err := mgr.TransitionStatus(ctx, ses.ID, domain.SessionActive)
	if err != nil {
		t.Fatalf("TransitionStatus Dormant→Active: %v", err)
	}
}

func TestSessionManager_Transition_DormantToCompleted(t *testing.T) {
	store := newStubSessionStore()
	mgr := New(store)

	ctx := context.Background()
	ses, _ := mgr.CreateSession(ctx, "test", "")
	_ = mgr.TransitionStatus(ctx, ses.ID, domain.SessionDormant)

	err := mgr.TransitionStatus(ctx, ses.ID, domain.SessionCompleted)
	if err != nil {
		t.Fatalf("TransitionStatus Dormant→Completed: %v", err)
	}
}

func TestSessionManager_Transition_Invalid_DormantToPaused(t *testing.T) {
	store := newStubSessionStore()
	mgr := New(store)

	ctx := context.Background()
	ses, _ := mgr.CreateSession(ctx, "test", "")
	_ = mgr.TransitionStatus(ctx, ses.ID, domain.SessionDormant)

	err := mgr.TransitionStatus(ctx, ses.ID, domain.SessionPaused)
	if err == nil {
		t.Fatal("expected error for Dormant→Paused transition")
	}
}

func TestSessionManager_Transition_Invalid_CompletedToActive(t *testing.T) {
	store := newStubSessionStore()
	mgr := New(store)

	ctx := context.Background()
	ses, _ := mgr.CreateSession(ctx, "test", "")
	_ = mgr.TransitionStatus(ctx, ses.ID, domain.SessionCompleted)

	err := mgr.TransitionStatus(ctx, ses.ID, domain.SessionActive)
	if err == nil {
		t.Fatal("expected error for Completed→Active transition")
	}
}

func TestSessionManager_Transition_SameStatus(t *testing.T) {
	store := newStubSessionStore()
	mgr := New(store)

	ctx := context.Background()
	ses, _ := mgr.CreateSession(ctx, "test", "")

	err := mgr.TransitionStatus(ctx, ses.ID, domain.SessionActive)
	if err != nil {
		t.Fatalf("same-status transition should be idempotent: %v", err)
	}
}

func TestSessionManager_ListSessions_FilterByStatus(t *testing.T) {
	store := newStubSessionStore()
	mgr := New(store)

	ctx := context.Background()
	ses1, _ := mgr.CreateSession(ctx, "goal 1", "")
	ses2, _ := mgr.CreateSession(ctx, "goal 2", "")
	_ = mgr.TransitionStatus(ctx, ses2.ID, domain.SessionCompleted)

	active, _ := mgr.ListSessions(ctx, domain.SessionActive)
	if len(active) != 1 {
		t.Errorf("expected 1 active session, got %d", len(active))
	}
	if len(active) > 0 && active[0].ID != ses1.ID {
		t.Errorf("expected ses1, got %s", active[0].ID)
	}

	completed, _ := mgr.ListSessions(ctx, domain.SessionCompleted)
	if len(completed) != 1 {
		t.Errorf("expected 1 completed session, got %d", len(completed))
	}

	all, _ := mgr.ListSessions(ctx, "")
	if len(all) != 2 {
		t.Errorf("expected 2 total sessions, got %d", len(all))
	}
}
