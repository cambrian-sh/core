package agentmgr

import (
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// capturingEventBus records DaemonCrashedEvents for assertions.
type capturingEventBus struct {
	mu     sync.Mutex
	events []domain.DaemonCrashedEvent
}

func (b *capturingEventBus) Subscribe(eventType string, handler func(domain.DomainEvent)) {}
func (b *capturingEventBus) Publish(e domain.DomainEvent) error {
	if evt, ok := e.(domain.DaemonCrashedEvent); ok {
		b.mu.Lock()
		b.events = append(b.events, evt)
		b.mu.Unlock()
	}
	return nil
}

// Cycle 1 — Marking a stop as expected prevents DaemonCrashedEvent on exit.
func TestCrashDetection_ExpectedStop_NoCrashEvent(t *testing.T) {
	m := newTestManager()
	bus := &capturingEventBus{}
	m.EventBus = bus

	inst := domain.NewInstance("gold-tracker")
	inst.Mode = domain.ModeDaemon

	// Mark as expected stop (as StopDaemon does before Kill).
	m.markExpectedStop(inst.ID)

	// Simulate the crash watcher seeing an exit from an "expected" stop.
	m.handleDaemonExit(inst, "gold-tracker", false /* unexpected=false */)

	time.Sleep(20 * time.Millisecond)
	bus.mu.Lock()
	n := len(bus.events)
	bus.mu.Unlock()
	if n > 0 {
		t.Errorf("expected stop must NOT publish DaemonCrashedEvent, got %d events", n)
	}
}

// Cycle 2 — Unexpected exit publishes DaemonCrashedEvent.
func TestCrashDetection_UnexpectedExit_PublishesCrashEvent(t *testing.T) {
	m := newTestManager()
	bus := &capturingEventBus{}
	m.EventBus = bus

	inst := domain.NewInstance("gold-tracker")
	inst.Mode = domain.ModeDaemon

	// NOT marked as expected — simulates process crashing.
	m.handleDaemonExit(inst, "gold-tracker", true /* unexpected */)

	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		bus.mu.Lock()
		n := len(bus.events)
		bus.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	bus.mu.Lock()
	events := bus.events
	bus.mu.Unlock()

	if len(events) != 1 {
		t.Fatalf("want 1 DaemonCrashedEvent, got %d", len(events))
	}
	if events[0].AgentID != "gold-tracker" {
		t.Errorf("AgentID: want %q, got %q", "gold-tracker", events[0].AgentID)
	}
	if events[0].StreamID != "gold-tracker" {
		t.Errorf("StreamID: want %q, got %q", "gold-tracker", events[0].StreamID)
	}
}

// Cycle 3 — isExpectedStop returns false after markExpectedStop is consumed once.
func TestCrashDetection_ExpectedStop_ConsumedOnce(t *testing.T) {
	m := newTestManager()
	m.markExpectedStop("inst-abc")
	if !m.isExpectedStop("inst-abc") {
		t.Error("first check should be expected")
	}
	// Second check: consumed.
	if m.isExpectedStop("inst-abc") {
		t.Error("second check should not be expected (consumed)")
	}
}
