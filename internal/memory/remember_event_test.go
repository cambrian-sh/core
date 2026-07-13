package memory

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// A successful Remember publishes a MemoryWrittenEvent carrying the new doc id.
// ADR-0047 0047-15.
func TestRemember_PublishesMemoryWrittenEvent(t *testing.T) {
	store := &capturingSaveStore{}
	bus := domain.NewInMemoryEventBus()
	var events []domain.MemoryWrittenEvent
	bus.Subscribe(domain.EventTypeMemoryWritten, func(e domain.DomainEvent) {
		events = append(events, e.(domain.MemoryWrittenEvent))
	})

	svc := NewRememberService(store, &fakeEmbedder{}, fakeWriteResolver{known: map[string]bool{"agent-1": true}})
	svc.SetEventBus(bus)

	docID, err := svc.Remember(context.Background(), "agent-1", "the sky is blue", nil, "src", "sess-9", 0)
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected one MemoryWrittenEvent, got %d", len(events))
	}
	e := events[0]
	if e.DocID != docID || e.SessionID != "sess-9" || e.Source != "src" || e.DocType != string(domain.DocTypeMnemonicFact) {
		t.Fatalf("event fields mismatch: %+v (docID=%s)", e, docID)
	}
	if e.Summary == "" {
		t.Fatal("expected a non-empty summary")
	}
}

// A rejected write (unknown principal) publishes nothing.
func TestRemember_NoEventOnRejectedWrite(t *testing.T) {
	bus := domain.NewInMemoryEventBus()
	var count int
	bus.Subscribe(domain.EventTypeMemoryWritten, func(domain.DomainEvent) { count++ })

	svc := NewRememberService(&capturingSaveStore{}, &fakeEmbedder{}, fakeWriteResolver{known: map[string]bool{}})
	svc.SetEventBus(bus)

	if _, err := svc.Remember(context.Background(), "ghost", "x", nil, "s", "sess", 0); err == nil {
		t.Fatal("expected ErrUnknownPrincipal")
	}
	if count != 0 {
		t.Fatalf("rejected write must not publish, got %d events", count)
	}
}
