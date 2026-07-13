package synaptic

import (
	"context"
	"sort"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// EventStore is the persistence interface for session events.
type EventStore interface {
	LogEvent(ctx context.Context, ev domain.SessionEvent) error
	GetEvents(ctx context.Context, sessionID string, limit int) ([]domain.SessionEvent, error)
	GetEventsByType(ctx context.Context, sessionID string, types ...domain.SessionEventType) ([]domain.SessionEvent, error)
}

// EventLogger writes and reads session events through the EventStore.
type EventLogger struct {
	store EventStore
}

func New(store EventStore) *EventLogger {
	return &EventLogger{store: store}
}

// LogEvent writes a session event with the current timestamp.
func (l *EventLogger) LogEvent(ctx context.Context, sessionID string, eventType domain.SessionEventType, payload string, artifactIDs []string) error {
	if artifactIDs == nil {
		artifactIDs = []string{}
	}
	ev := domain.SessionEvent{
		SessionID:   sessionID,
		Type:        eventType,
		Timestamp:   time.Now(),
		Payload:     payload,
		ArtifactIDs: artifactIDs,
	}
	return l.store.LogEvent(ctx, ev)
}

// GetRecentEvents returns the most recent N events for a session.
func (l *EventLogger) GetRecentEvents(ctx context.Context, sessionID string, limit int) ([]domain.SessionEvent, error) {
	events, err := l.store.GetEvents(ctx, sessionID, limit)
	if err != nil {
		return nil, err
	}
	sort.Slice(events, func(i, j int) bool {
		return events[i].Timestamp.After(events[j].Timestamp)
	})
	return events, nil
}

// GetEventsByType returns events of the specified types for a session.
func (l *EventLogger) GetEventsByType(ctx context.Context, sessionID string, types ...domain.SessionEventType) ([]domain.SessionEvent, error) {
	return l.store.GetEventsByType(ctx, sessionID, types...)
}

// Close is a lifecycle hook for future resource cleanup.
func (l *EventLogger) Close() error {
	return nil
}
