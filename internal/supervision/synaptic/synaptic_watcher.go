package synaptic

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// EventStore is the consumer-side interface SynapticWatcher needs for event access.
type EventStore interface {
	LogEvent(ctx context.Context, ev domain.SessionEvent) error
	GetEvents(ctx context.Context, sessionID string, limit int) ([]domain.SessionEvent, error)
	GetEventsByType(ctx context.Context, sessionID string, types ...domain.SessionEventType) ([]domain.SessionEvent, error)
	GetAllRecentEvents(ctx context.Context, since time.Time, limit int) ([]domain.SessionEvent, error)
}

// MemoryIngester is the interface for storing memories to LTM.
type MemoryIngester interface {
	IngestSync(ctx context.Context, text, source string) error
}

// SynapticWatcher tails the event log and ingests high-priority events to LTM.
// It is the system's Amygdala — deciding which signals are significant enough
// to move from Short-term Buffer (BBolt) to Long-term Memory (pgvector).
type SynapticWatcher struct {
	EventStore     EventStore
	MemoryIngester MemoryIngester
	PollInterval   time.Duration

	lastProcessed time.Time
	stop          context.CancelFunc
}

func New(store EventStore, mem MemoryIngester) *SynapticWatcher {
	return &SynapticWatcher{
		EventStore:     store,
		MemoryIngester: mem,
		PollInterval:   time.Second,
	}
}

// Start launches the background observation loop.
func (w *SynapticWatcher) Start(ctx context.Context) {
	ctx, w.stop = context.WithCancel(ctx)
	slog.Info("SynapticWatcher: starting observation loop", "interval", w.PollInterval)
	ticker := time.NewTicker(w.PollInterval)
	defer ticker.Stop()

	w.lastProcessed = time.Now().Add(-1 * time.Minute)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.processNewEvents(ctx)
		}
	}
}

// Stop signals the observation loop to exit.
func (w *SynapticWatcher) Stop() {
	if w.stop != nil {
		w.stop()
	}
}

func (w *SynapticWatcher) processNewEvents(ctx context.Context) {
	since := w.lastProcessed
	w.lastProcessed = time.Now()

	events, err := w.EventStore.GetAllRecentEvents(ctx, since, 100)
	if err != nil {
		slog.Warn("SynapticWatcher: failed to fetch events", "err", err)
		return
	}
	if len(events) == 0 {
		return
	}

	if pattern := detectCrossEventPattern(events); pattern != "" {
		if err := w.MemoryIngester.IngestSync(ctx, pattern, "synaptic_watcher:pattern"); err != nil {
			slog.Warn("SynapticWatcher: pattern ingest failed", "err", err)
		}
	}

	for _, ev := range events {
		priority := eventPriority(ev.Type)
		if !shouldIngest(priority) {
			continue
		}
		text := fmt.Sprintf("[%s] %s", ev.Type, ev.Payload)
		if err := w.MemoryIngester.IngestSync(ctx, text, "synaptic_watcher:"+string(ev.Type)); err != nil {
			slog.Warn("SynapticWatcher: event ingest failed", "type", ev.Type, "err", err)
		}
	}
}

// eventPriority returns the heuristic priority score for an event type.
func eventPriority(t domain.SessionEventType) int {
	switch t {
	case domain.EventCriticalError:
		return 10
	case domain.EventHITLIntervention:
		return 8
	case domain.EventBudgetBreach:
		return 7
	case domain.EventUserMessage:
		return 5
	case domain.EventSystemThought:
		return 3
	default:
		return 1
	}
}

// shouldIngest returns true if the event's priority exceeds the LTM threshold.
func shouldIngest(priority int) bool { return priority >= 7 }

// detectCrossEventPattern looks for patterns in a window of events.
func detectCrossEventPattern(events []domain.SessionEvent) string {
	if len(events) < 2 {
		return ""
	}
	var builder strings.Builder
	errorCount := 0
	for _, ev := range events {
		if ev.Type == domain.EventCriticalError {
			errorCount++
		}
	}
	if errorCount >= 2 {
		builder.WriteString("Repeated failures detected: ")
		builder.WriteString("multiple CriticalError events occurred in close succession. ")
	}
	if errorCount > 0 {
		for i := 0; i < len(events)-1; i++ {
			if events[i].Type == domain.EventCriticalError &&
				events[i+1].Type == domain.EventUserMessage {
				builder.WriteString("User expressed frustration after agent failure. ")
				break
			}
		}
	}
	return builder.String()
}

