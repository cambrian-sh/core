package domain_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// Cycle 1 — Subscribe + Publish delivers the event to the handler.
func TestInMemoryEventBus_PublishDeliversToSubscriber(t *testing.T) {
	bus := domain.NewInMemoryEventBus()

	var got domain.DomainEvent
	bus.Subscribe(domain.EventTypeAgentReady, func(e domain.DomainEvent) { got = e })

	want := domain.AgentReadyEvent{AgentID: "agent-1"}
	if err := bus.Publish(want); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	ready, ok := got.(domain.AgentReadyEvent)
	if !ok || ready.AgentID != "agent-1" {
		t.Fatalf("expected AgentReadyEvent{agent-1}, got %v", got)
	}
}

// Cycle 2 — Multiple subscribers for the same type all receive the event.
func TestInMemoryEventBus_MultipleSubscribersAllReceive(t *testing.T) {
	bus := domain.NewInMemoryEventBus()

	var count int32
	inc := func(_ domain.DomainEvent) { atomic.AddInt32(&count, 1) }
	bus.Subscribe(domain.EventTypeAgentReady, inc)
	bus.Subscribe(domain.EventTypeAgentReady, inc)
	bus.Subscribe(domain.EventTypeAgentReady, inc)

	_ = bus.Publish(domain.AgentReadyEvent{AgentID: "a"})

	if got := atomic.LoadInt32(&count); got != 3 {
		t.Fatalf("expected 3 handlers called, got %d", got)
	}
}

// Cycle 3 — Publishing type A does not reach a subscriber registered for type B.
func TestInMemoryEventBus_TypeRoutingIsIsolated(t *testing.T) {
	bus := domain.NewInMemoryEventBus()

	var auctionCalled bool
	bus.Subscribe(domain.EventTypeAuctionEvent, func(_ domain.DomainEvent) { auctionCalled = true })

	_ = bus.Publish(domain.AgentReadyEvent{AgentID: "x"}) // different type

	if auctionCalled {
		t.Fatal("auction subscriber should not fire for AgentReadyEvent")
	}
}

// Cycle 4 — Concurrent publishes from multiple goroutines do not race.
func TestInMemoryEventBus_ConcurrentPublishIsSafe(t *testing.T) {
	bus := domain.NewInMemoryEventBus()

	var count int32
	bus.Subscribe(domain.EventTypeAgentReady, func(_ domain.DomainEvent) {
		atomic.AddInt32(&count, 1)
	})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = bus.Publish(domain.AgentReadyEvent{})
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&count); got != 50 {
		t.Fatalf("expected 50 deliveries, got %d", got)
	}
}

// Cycle 5 — Publish with no subscriber registered does not error.
func TestInMemoryEventBus_PublishWithNoSubscriberIsNoError(t *testing.T) {
	bus := domain.NewInMemoryEventBus()
	if err := bus.Publish(domain.AgentReadyEvent{AgentID: "none"}); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
}

// Cycle 6 — Subscribe after Publish does not receive the already-published event.
func TestInMemoryEventBus_LateSubscriberMissesEarlierEvent(t *testing.T) {
	bus := domain.NewInMemoryEventBus()

	_ = bus.Publish(domain.AgentReadyEvent{AgentID: "early"})

	called := false
	bus.Subscribe(domain.EventTypeAgentReady, func(_ domain.DomainEvent) { called = true })

	// Give any hypothetical async delivery a moment.
	time.Sleep(10 * time.Millisecond)
	if called {
		t.Fatal("late subscriber should not receive an event published before it registered")
	}
}
