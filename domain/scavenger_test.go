package domain_test

import (
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// captureEventBus is a shared test double used across domain_test files.
type captureEventBus struct {
	events []domain.DomainEvent
}

func (b *captureEventBus) Subscribe(_ string, _ domain.EventHandler) {}
func (b *captureEventBus) Publish(e domain.DomainEvent) error {
	b.events = append(b.events, e)
	return nil
}

// Cycle 1 — OnDocumentCommitted publishes MemoryPressureEvent when over threshold.
func TestScavenger_OnDocumentCommitted_PublishesPressureEvent(t *testing.T) {
	bus := &captureEventBus{}
	s := &domain.MemoryPressureScavenger{
		MaxOrphanedDocuments: 100,
		Bus:                  bus,
	}

	s.OnDocumentCommitted(domain.MemoryMetrics{OrphanedDocuments: 150})

	if len(bus.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(bus.events))
	}
	if _, ok := bus.events[0].(domain.MemoryPressureEvent); !ok {
		t.Fatalf("expected MemoryPressureEvent, got %T", bus.events[0])
	}
}

// Cycle 2 — OnDocumentCommitted does NOT publish when below threshold.
func TestScavenger_OnDocumentCommitted_BelowThreshold_NoEvent(t *testing.T) {
	bus := &captureEventBus{}
	s := &domain.MemoryPressureScavenger{
		MaxOrphanedDocuments: 100,
		Bus:                  bus,
	}

	s.OnDocumentCommitted(domain.MemoryMetrics{OrphanedDocuments: 50})

	if len(bus.events) != 0 {
		t.Fatalf("expected no events, got %d", len(bus.events))
	}
}

// Cycle 3 — Zero threshold disables orphaned document pressure checking.
func TestScavenger_ZeroThreshold_NeverPublishesPressure(t *testing.T) {
	bus := &captureEventBus{}
	s := &domain.MemoryPressureScavenger{
		MaxOrphanedDocuments: 0, // disabled
		Bus:                  bus,
	}

	s.OnDocumentCommitted(domain.MemoryMetrics{OrphanedDocuments: 999999})

	if len(bus.events) != 0 {
		t.Fatalf("expected no events, got %d", len(bus.events))
	}
}

// Cycle 4 — IndexSizeBytes threshold also triggers when exceeded.
func TestScavenger_IndexSizeThreshold_Triggers(t *testing.T) {
	bus := &captureEventBus{}
	const maxBytes = 1024 * 1024
	s := &domain.MemoryPressureScavenger{
		MaxIndexSizeBytes: maxBytes,
		Bus:               bus,
	}

	s.OnDocumentCommitted(domain.MemoryMetrics{IndexSizeBytes: maxBytes + 1})

	if len(bus.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(bus.events))
	}
}

// Cycle 5 — Nil bus is safe.
func TestScavenger_NilBus_IsSafe(t *testing.T) {
	s := &domain.MemoryPressureScavenger{
		MaxOrphanedDocuments: 10,
		Bus:                  nil,
	}
	// Should not panic.
	s.OnDocumentCommitted(domain.MemoryMetrics{OrphanedDocuments: 999})
}
