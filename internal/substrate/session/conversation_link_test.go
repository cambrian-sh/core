package session

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// A session opened by a chat turn records which conversation — and which TURN — ordered it.
// ADR-0084 D2 specified this relationship and neither entity referenced the other, so
// nothing could answer "what did this conversation set in motion?".
func TestSaveConversationLink_AttributesTheOrderingTurn(t *testing.T) {
	m, store, bus := newLifecycleMgr(t)
	ctx := context.Background()
	ses, _ := m.CreateSession(ctx, "book a flight", "")

	if err := m.SaveConversationLink(ctx, ses.ID, "conv-1", "msg-7"); err != nil {
		t.Fatalf("link: %v", err)
	}

	got := store.sessions[ses.ID]
	if got.ConversationID != "conv-1" {
		t.Errorf("ConversationID = %q, want conv-1", got.ConversationID)
	}
	if got.OriginMessageID != "msg-7" {
		t.Errorf("OriginMessageID = %q, want msg-7", got.OriginMessageID)
	}
	// The link is observable on the feed, so a console can show it without polling.
	states := bus.states()
	if len(states) < 2 {
		t.Fatalf("linking should publish state, got %d events", len(states))
	}
}

// Write-once: a session belongs to the turn that STARTED it. Causation does not change
// retroactively, so a later turn touching the same session must not rewrite its origin.
func TestSaveConversationLink_IsWriteOnce(t *testing.T) {
	m, store, _ := newLifecycleMgr(t)
	ctx := context.Background()
	ses, _ := m.CreateSession(ctx, "goal", "")

	if err := m.SaveConversationLink(ctx, ses.ID, "conv-FIRST", "msg-1"); err != nil {
		t.Fatalf("first link: %v", err)
	}
	if err := m.SaveConversationLink(ctx, ses.ID, "conv-SECOND", "msg-2"); err != nil {
		t.Fatalf("second link: %v", err)
	}

	got := store.sessions[ses.ID]
	if got.ConversationID != "conv-FIRST" || got.OriginMessageID != "msg-1" {
		t.Errorf("origin was rewritten: got %q/%q, want conv-FIRST/msg-1",
			got.ConversationID, got.OriginMessageID)
	}
}

// An empty conversation is a no-op, not an error: most sessions are not chat-ordered.
func TestSaveConversationLink_EmptyConversationIsNoOp(t *testing.T) {
	m, store, _ := newLifecycleMgr(t)
	ctx := context.Background()
	ses, _ := m.CreateSession(ctx, "goal", "")

	if err := m.SaveConversationLink(ctx, ses.ID, "", ""); err != nil {
		t.Fatalf("empty link should be a no-op: %v", err)
	}
	if got := store.sessions[ses.ID]; got.ConversationID != "" {
		t.Errorf("expected no link, got %q", got.ConversationID)
	}
}

func TestSaveConversationLink_UnknownSessionErrors(t *testing.T) {
	m, _, _ := newLifecycleMgr(t)
	if err := m.SaveConversationLink(context.Background(), "nope", "conv-1", "msg-1"); err == nil {
		t.Fatal("linking an unknown session must fail, not silently succeed")
	}
}

// A store that cannot index the link says so explicitly, so a caller can tell "no sessions"
// from "this backend cannot answer that".
func TestListSessionsForConversation_UnsupportedStoreIsExplicit(t *testing.T) {
	m, _, _ := newLifecycleMgr(t) // stubSessionStore does not implement the lister
	_, err := m.ListSessionsForConversation(context.Background(), "conv-1")
	if !errors.Is(err, ErrConversationIndexUnsupported) {
		t.Fatalf("expected ErrConversationIndexUnsupported, got %v", err)
	}
}

// A store that CAN index it is used.
type listerStore struct {
	*stubSessionStore
	byConversation map[string][]domain.Session
}

func (l *listerStore) ListSessionsForConversation(_ context.Context, id string) ([]domain.Session, error) {
	return l.byConversation[id], nil
}

func TestListSessionsForConversation_UsesTheStoreWhenSupported(t *testing.T) {
	store := &listerStore{
		stubSessionStore: newStubSessionStore(),
		byConversation: map[string][]domain.Session{
			"conv-1": {{ID: "s1"}, {ID: "s2"}},
		},
	}
	m := New(store)

	got, err := m.ListSessionsForConversation(context.Background(), "conv-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 sessions for the conversation, got %d", len(got))
	}
}
