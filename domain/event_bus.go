package domain

import (
	"context"
	"sync"
)

// EventHandler is invoked synchronously by EventBus.Publish for each subscriber
// registered under the matching event type. ctx is the publisher's context
// (AGENTS.md §2.7, ENG-1466): a handler that does I/O runs under the caller's
// deadline, cancellation and scope. A handler that hands the event to work
// outliving the Publish call detaches with context.WithoutCancel itself.
type EventHandler func(ctx context.Context, event DomainEvent)

// EventBus is the internal pub/sub contract for system events.
// It replaces the TUI-coupled EventSink (deleted with ADR-0030).
// Implementations must be safe for concurrent use from multiple goroutines.
type EventBus interface {
	// Subscribe registers handler to be called whenever an event of the given
	// type is published. Subscriptions persist for the lifetime of the bus.
	Subscribe(eventType string, handler EventHandler)

	// Publish delivers event to every handler registered for event.EventType().
	// Returns an error only if the bus itself fails (e.g. shut down); handler
	// panics are the caller's responsibility. ctx is handed to every handler.
	Publish(ctx context.Context, event DomainEvent) error
}

// InMemoryEventBus is a synchronous, goroutine-safe EventBus backed by an
// in-memory map. Handlers are called in subscription order within the
// Publish call — no separate goroutines. This keeps the delivery model
// simple and avoids hidden goroutine leaks.
type InMemoryEventBus struct {
	mu   sync.RWMutex
	subs map[string][]EventHandler
}

// NewInMemoryEventBus constructs a ready-to-use InMemoryEventBus.
func NewInMemoryEventBus() *InMemoryEventBus {
	return &InMemoryEventBus{subs: make(map[string][]EventHandler)}
}

// Subscribe implements EventBus.
func (b *InMemoryEventBus) Subscribe(eventType string, handler EventHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[eventType] = append(b.subs[eventType], handler)
}

// Publish implements EventBus. It copies the handler slice under a read-lock
// so Publish and Subscribe can be called concurrently without deadlock.
func (b *InMemoryEventBus) Publish(ctx context.Context, event DomainEvent) error {
	b.mu.RLock()
	handlers := make([]EventHandler, len(b.subs[event.EventType()]))
	copy(handlers, b.subs[event.EventType()])
	b.mu.RUnlock()

	for _, h := range handlers {
		h(ctx, event)
	}
	return nil
}
