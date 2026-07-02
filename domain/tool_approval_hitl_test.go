package domain

import (
	"context"
	"testing"
	"time"
)

// A raised dangerous-tool approval publishes a HITLRaisedEvent keyed by the same
// id Submit/ResolveHITL use. ADR-0047 0047-19.
func TestApprovalController_PublishesHITLRaised(t *testing.T) {
	c := NewInMemoryApprovalController(2 * time.Second)
	bus := NewInMemoryEventBus()
	var got []HITLRaisedEvent
	bus.Subscribe(EventTypeHITLRaised, func(e DomainEvent) {
		got = append(got, e.(HITLRaisedEvent))
	})
	c.Bus = bus

	sub, cancel := c.Watch() // a subscriber must exist or Request fails closed
	defer cancel()

	done := make(chan ApprovalDecision, 1)
	go func() {
		dec, _ := c.Request(context.Background(), ApprovalRequest{AgentID: "a1", ToolName: "execute_command"})
		done <- dec
	}()

	// The event is published before subscriber delivery, so receiving on the Watch
	// channel guarantees the event already fired.
	select {
	case <-sub:
	case <-time.After(2 * time.Second):
		t.Fatal("approval request was not delivered")
	}

	if len(got) != 1 {
		t.Fatalf("expected one HITLRaisedEvent, got %d", len(got))
	}
	if got[0].AgentID != "a1" || !got[0].IsDestructive || got[0].InterventionID == "" {
		t.Fatalf("unexpected HITL event: %+v", got[0])
	}

	// The published id resolves the pending request (raise/resolve agree).
	if !c.Submit(got[0].InterventionID, true, "op") {
		t.Fatal("Submit with the published intervention id should resolve the request")
	}
	if dec := <-done; !dec.Approved {
		t.Fatal("expected the request to be approved")
	}
}
