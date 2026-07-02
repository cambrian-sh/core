package llm

import (
	"testing"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// The breaker publishes an LLMHealthEvent on each open↔closed transition and
// nothing on a same-state Record. ADR-0047 0047-14.
func TestCircuitBreaker_PublishesHealthOnTransition(t *testing.T) {
	bus := domain.NewInMemoryEventBus()
	var events []domain.LLMHealthEvent
	bus.Subscribe(domain.EventTypeLLMHealth, func(e domain.DomainEvent) {
		events = append(events, e.(domain.LLMHealthEvent))
	})

	b := NewCircuitBreaker(2, 50*time.Millisecond)
	b.Bus = bus

	b.Record("m1", false) // failure 1 — still healthy, no transition
	if len(events) != 0 {
		t.Fatalf("no transition expected yet, got %d events", len(events))
	}

	b.Record("m1", false) // failure 2 — trips OPEN
	if len(events) != 1 || events[0].State != "open" || events[0].ModelID != "m1" {
		t.Fatalf("expected one open event for m1, got %+v", events)
	}

	b.Record("m1", true) // recovers — CLOSED
	if len(events) != 2 || events[1].State != "closed" {
		t.Fatalf("expected a closed event, got %+v", events)
	}

	b.Record("m1", true) // already closed — no event
	if len(events) != 2 {
		t.Fatalf("same-state Record must not publish, got %d events", len(events))
	}
}

// A nil bus is a no-op (breaker still functions).
func TestCircuitBreaker_NilBusIsNoOp(t *testing.T) {
	b := NewCircuitBreaker(1, time.Second) // no Bus set
	b.Record("m1", false)                  // trips, would publish if a bus were set
	if b.Healthy("m1") {
		t.Fatal("expected m1 to be open after a failure at threshold 1")
	}
}
