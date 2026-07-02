package synaptic

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

type stubEventStore struct {
	events []domain.SessionEvent
}

func (s *stubEventStore) LogEvent(_ context.Context, ev domain.SessionEvent) error {
	s.events = append(s.events, ev)
	return nil
}

func (s *stubEventStore) GetEvents(_ context.Context, sessionID string, limit int) ([]domain.SessionEvent, error) {
	var out []domain.SessionEvent
	for i := len(s.events) - 1; i >= 0 && len(out) < limit; i-- {
		if s.events[i].SessionID == sessionID {
			out = append(out, s.events[i])
		}
	}
	return out, nil
}

func (s *stubEventStore) GetEventsByType(_ context.Context, sessionID string, types ...domain.SessionEventType) ([]domain.SessionEvent, error) {
	typeSet := make(map[domain.SessionEventType]bool, len(types))
	for _, t := range types {
		typeSet[t] = true
	}
	var out []domain.SessionEvent
	for _, ev := range s.events {
		if ev.SessionID == sessionID && typeSet[ev.Type] {
			out = append(out, ev)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Timestamp.Before(out[j].Timestamp)
	})
	return out, nil
}

func TestEventLogger_LogEvent_StoresEvent(t *testing.T) {
	store := &stubEventStore{}
	logger := New(store)

	err := logger.LogEvent(context.Background(), "sess-1", domain.EventUserMessage, "hello", nil)
	if err != nil {
		t.Fatalf("LogEvent: %v", err)
	}
	if len(store.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(store.events))
	}
	if store.events[0].SessionID != "sess-1" {
		t.Errorf("SessionID: got %q", store.events[0].SessionID)
	}
	if store.events[0].Type != domain.EventUserMessage {
		t.Errorf("Type: got %q", store.events[0].Type)
	}
	if store.events[0].Payload != "hello" {
		t.Errorf("Payload: got %q", store.events[0].Payload)
	}
}

func TestEventLogger_LogEvent_SetsTimestamp(t *testing.T) {
	store := &stubEventStore{}
	logger := New(store)

	logger.LogEvent(context.Background(), "s1", domain.EventSystemThought, "thought", nil)
	if store.events[0].Timestamp.IsZero() {
		t.Error("timestamp is zero")
	}
}

func TestEventLogger_LogEvent_WithArtifactIDs(t *testing.T) {
	store := &stubEventStore{}
	logger := New(store)

	artifacts := []string{"hash-a", "hash-b"}
	logger.LogEvent(context.Background(), "s1", domain.EventSystemThought, "done", artifacts)

	if len(store.events[0].ArtifactIDs) != 2 {
		t.Fatalf("expected 2 artifact IDs, got %d", len(store.events[0].ArtifactIDs))
	}
}

func TestEventLogger_GetRecentEvents_ReturnsLatest(t *testing.T) {
	store := &stubEventStore{}
	logger := New(store)

	logger.LogEvent(context.Background(), "s1", domain.EventUserMessage, "msg1", nil)
	time.Sleep(time.Millisecond)
	logger.LogEvent(context.Background(), "s1", domain.EventUserMessage, "msg2", nil)
	time.Sleep(time.Millisecond)
	logger.LogEvent(context.Background(), "s1", domain.EventUserMessage, "msg3", nil)

	recent, err := logger.GetRecentEvents(context.Background(), "s1", 2)
	if err != nil {
		t.Fatalf("GetRecentEvents: %v", err)
	}
	if len(recent) != 2 {
		t.Fatalf("expected 2 events, got %d", len(recent))
	}
	if recent[0].Payload != "msg3" {
		t.Errorf("first recent should be msg3, got %q", recent[0].Payload)
	}
}

func TestEventLogger_GetEventsByType_FiltersCorrectly(t *testing.T) {
	store := &stubEventStore{}
	logger := New(store)

	logger.LogEvent(context.Background(), "s1", domain.EventUserMessage, "user msg", nil)
	logger.LogEvent(context.Background(), "s1", domain.EventCriticalError, "error", nil)
	logger.LogEvent(context.Background(), "s1", domain.EventSystemThought, "thought", nil)

	errors, _ := logger.GetEventsByType(context.Background(), "s1", domain.EventCriticalError)
	if len(errors) != 1 {
		t.Fatalf("expected 1 error event, got %d", len(errors))
	}

	thoughts, _ := logger.GetEventsByType(context.Background(), "s1", domain.EventSystemThought)
	if len(thoughts) != 1 {
		t.Fatalf("expected 1 thought event, got %d", len(thoughts))
	}
}

func TestEventLogger_LogEvent_NilArtifactIDs_BecomesEmpty(t *testing.T) {
	store := &stubEventStore{}
	logger := New(store)

	logger.LogEvent(context.Background(), "s1", domain.EventCheckpointSaved, "cp", nil)
	if len(store.events[0].ArtifactIDs) != 0 {
		t.Errorf("expected empty artifact IDs, got %v", store.events[0].ArtifactIDs)
	}
}
