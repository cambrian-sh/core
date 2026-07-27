package chat

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// recordingDeliverer stands in for internal/ingress.DeliveryService.
type recordingDeliverer struct {
	mu   sync.Mutex
	sent []struct{ conv, text, txn string }
	err  error
}

func (d *recordingDeliverer) Deliver(_ context.Context, conv, text, txn string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return d.err
	}
	d.sent = append(d.sent, struct{ conv, text, txn string }{conv, text, txn})
	return nil
}

func (d *recordingDeliverer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.sent)
}

func emitService(t *testing.T) (*TurnService, *memStore, *recordingDeliverer) {
	t.Helper()
	store := newMemStore()
	if err := store.CreateConversation(context.Background(), domain.Conversation{
		ID: "conv-7", OwnerID: "alice", Status: domain.ConversationOpen, Profile: domain.ProfileCustomer,
	}); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	d := &recordingDeliverer{}
	s := NewTurnService(store, nil, nil)
	s.SetDeliverer(d)
	return s, store, d
}

// The whole point of the primitive: one inbound message no longer implies one
// outbound message. An agent may say "checking..." and then answer.
func TestEmit_SpeaksMoreThanOnce(t *testing.T) {
	s, store, d := emitService(t)
	ctx := context.Background()

	first, err := s.Emit(ctx, "conv-7", "Checking now...")
	if err != nil {
		t.Fatalf("first emit: %v", err)
	}
	second, err := s.Emit(ctx, "conv-7", "3 options, from 89 EUR")
	if err != nil {
		t.Fatalf("second emit: %v", err)
	}

	if d.count() != 2 {
		t.Fatalf("expected 2 deliveries, got %d", d.count())
	}
	if d.sent[0].text != "Checking now..." || d.sent[1].text != "3 options, from 89 EUR" {
		t.Errorf("order was not preserved: %+v", d.sent)
	}
	// Seq is what guarantees the order downstream, and it was already race-tested.
	if second.Seq <= first.Seq {
		t.Errorf("Seq must advance: %d then %d", first.Seq, second.Seq)
	}
	// The idempotency key is the message id, so a retry cannot double-send.
	if d.sent[0].txn != first.ID || d.sent[1].txn != second.ID {
		t.Errorf("txn ids must be the message ids: %+v", d.sent)
	}
	if got := len(store.convs); got != 1 {
		t.Errorf("emitting must not create conversations, got %d", got)
	}
}

// Nobody asked. A request/response model has nowhere to put this.
func TestEmit_ProactiveWithNoInboundTurn(t *testing.T) {
	s, _, d := emitService(t)

	if _, err := s.Emit(context.Background(), "conv-7", "Your 14:00 is delayed to 16:30"); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if d.count() != 1 {
		t.Fatalf("a proactive message must be delivered, got %d", d.count())
	}
}

// A console-only conversation has nowhere to push to. Not an error — it is every
// conversation in a deployment with no ingress.
func TestEmit_NoDeliveryAddressIsNotAFailure(t *testing.T) {
	s, _, _ := emitService(t)
	s.SetDeliverer(&recordingDeliverer{err: domain.ErrNoDeliveryAddress})

	msg, err := s.Emit(context.Background(), "conv-7", "stored anyway")
	if err != nil {
		t.Fatalf("an unbound conversation must not make Emit fail: %v", err)
	}
	if msg.ID == "" {
		t.Error("the message must still be stored")
	}
}

// The stored message is the source of truth and is durable BEFORE delivery is
// attempted, so a delivery failure never loses what was said.
func TestEmit_DeliveryFailureStillStoresTheMessage(t *testing.T) {
	s, store, _ := emitService(t)
	s.SetDeliverer(&recordingDeliverer{err: errors.New("429 too many requests")})

	msg, err := s.Emit(context.Background(), "conv-7", "important")
	if err == nil {
		t.Fatal("a delivery failure must surface")
	}
	if msg.ID == "" {
		t.Fatal("the message must be stored even when delivery fails")
	}
	stored, lerr := store.ListMessages(context.Background(), "conv-7", 0, 0)
	if lerr != nil || len(stored) != 1 || stored[0].Content != "important" {
		t.Errorf("message not durable: %v %+v", lerr, stored)
	}
}

// With no deliverer the tier is store-only, which is exactly today's behaviour.
func TestEmit_WithoutADelivererIsStoreOnly(t *testing.T) {
	store := newMemStore()
	_ = store.CreateConversation(context.Background(), domain.Conversation{
		ID: "conv-7", OwnerID: "alice", Status: domain.ConversationOpen, Profile: domain.ProfileCustomer,
	})
	s := NewTurnService(store, nil, nil)

	msg, err := s.Emit(context.Background(), "conv-7", "hello")
	if err != nil || msg.ID == "" {
		t.Fatalf("store-only emit must work: %v", err)
	}
}

func TestEmit_RejectsEmptyText(t *testing.T) {
	s, _, _ := emitService(t)
	if _, err := s.Emit(context.Background(), "conv-7", "   "); err == nil {
		t.Error("an empty message would reach the far side as a blank line")
	}
}

func TestEmit_UnknownConversation(t *testing.T) {
	s, _, _ := emitService(t)
	if _, err := s.Emit(context.Background(), "nope", "hi"); !errors.Is(err, domain.ErrConversationNotFound) {
		t.Errorf("expected ErrConversationNotFound, got %v", err)
	}
}
