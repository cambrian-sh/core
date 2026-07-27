package ingress

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ── fakes ───────────────────────────────────────────────────────────────────

type fakeConvs map[string]domain.Conversation

func (f fakeConvs) GetConversation(_ context.Context, id string) (*domain.Conversation, error) {
	c, ok := f[id]
	if !ok {
		return nil, domain.ErrConversationNotFound
	}
	return &c, nil
}

type fakeIngresses map[string]domain.IngressRegistration

func (f fakeIngresses) ResolveIngress(_ context.Context, p domain.PrincipalRef) (domain.IngressRegistration, bool) {
	r, ok := f[p.ID]
	return r, ok
}

type fakeTransport struct {
	mu   sync.Mutex
	sent []domain.IngressDelivery
	err  error
}

func (f *fakeTransport) Deliver(_ context.Context, d domain.IngressDelivery) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, d)
	return nil
}

func (f *fakeTransport) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

type fakeJournal struct {
	seen    map[string]bool
	dead    []domain.UndeliverableDelivery
	markErr error
}

func newFakeJournal() *fakeJournal { return &fakeJournal{seen: map[string]bool{}} }

func (f *fakeJournal) MarkDeliveredOnce(key string) (bool, error) {
	if f.markErr != nil {
		return false, f.markErr
	}
	if f.seen[key] {
		return false, nil
	}
	f.seen[key] = true
	return true, nil
}

func (f *fakeJournal) RecordUndeliverable(d domain.UndeliverableDelivery) error {
	f.dead = append(f.dead, d)
	return nil
}

const convID = "conv-7"

func bound() fakeConvs {
	return fakeConvs{convID: {
		ID:       convID,
		Delivery: domain.DeliveryAddress{IngressAgentID: "telegram_ingress", ExternalID: "tg:12345"},
	}}
}

func registered() fakeIngresses {
	return fakeIngresses{"telegram_ingress": {
		AgentID:   "telegram_ingress",
		Surface:   domain.SurfaceRef{Kind: domain.SurfaceChat, ID: "telegram"},
		Namespace: []string{"tg:"},
	}}
}

// ── tests ───────────────────────────────────────────────────────────────────

// The caller names a CONVERSATION and the kernel supplies the recipient. Nothing
// upstream can choose who reads a message.
func TestDeliver_ResolvesTheRecipientFromTheConversation(t *testing.T) {
	tr := &fakeTransport{}
	s := NewDeliveryService(bound(), registered(), tr)

	if err := s.Deliver(context.Background(), convID, "Which dates?", "msg-1"); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if tr.count() != 1 {
		t.Fatalf("expected 1 delivery, got %d", tr.count())
	}
	got := tr.sent[0]
	if got.Address.ExternalID != "tg:12345" || got.Address.IngressAgentID != "telegram_ingress" {
		t.Errorf("address was not resolved from the conversation: %+v", got.Address)
	}
	if got.Text != "Which dates?" || got.ConversationID != convID {
		t.Errorf("delivery contents drifted: %+v", got)
	}
}

// A console-only conversation has nowhere to push to. Correct, not an error state
// to paper over — and it matches Telegram's own rule about cold-messaging.
func TestDeliver_UnboundConversationIsUndeliverable(t *testing.T) {
	tr := &fakeTransport{}
	s := NewDeliveryService(fakeConvs{convID: {ID: convID}}, registered(), tr)

	err := s.Deliver(context.Background(), convID, "hello", "msg-1")
	if !errors.Is(err, domain.ErrNoDeliveryAddress) {
		t.Fatalf("expected ErrNoDeliveryAddress, got %v", err)
	}
	if tr.count() != 0 {
		t.Error("nothing may be sent for an unbound conversation")
	}
}

// The point of re-checking at delivery time: revoking a compromised ingress has
// to stop conversations that were bound while it was still trusted. Checking only
// at bind time would leave every existing conversation working.
func TestDeliver_RevokedIngressStopsExistingConversations(t *testing.T) {
	tr := &fakeTransport{}
	s := NewDeliveryService(bound(), fakeIngresses{}, tr) // registration gone

	err := s.Deliver(context.Background(), convID, "hello", "msg-1")
	if !errors.Is(err, domain.ErrIngressRevoked) {
		t.Fatalf("expected ErrIngressRevoked, got %v", err)
	}
	if tr.count() != 0 {
		t.Error("a revoked ingress must not receive deliveries")
	}
}

// Narrowing a namespace is a partial revocation and must bite the same way.
func TestDeliver_NarrowedNamespaceStopsDelivery(t *testing.T) {
	tr := &fakeTransport{}
	narrowed := fakeIngresses{"telegram_ingress": {
		AgentID:   "telegram_ingress",
		Surface:   domain.SurfaceRef{Kind: domain.SurfaceChat, ID: "telegram"},
		Namespace: []string{"tgvip:"}, // no longer covers tg:12345
	}}
	s := NewDeliveryService(bound(), narrowed, tr)

	if err := s.Deliver(context.Background(), convID, "hello", "msg-1"); !errors.Is(err, domain.ErrIngressRevoked) {
		t.Fatalf("expected ErrIngressRevoked, got %v", err)
	}
	if tr.count() != 0 {
		t.Error("a recipient outside the current namespace must not be delivered to")
	}
}

// A retried delivery must not produce a second message on the far side.
func TestDeliver_IsIdempotentOnTxnID(t *testing.T) {
	tr := &fakeTransport{}
	s := NewDeliveryService(bound(), registered(), tr)
	s.SetJournal(newFakeJournal())

	for i := 0; i < 3; i++ {
		if err := s.Deliver(context.Background(), convID, "Checking now...", "msg-1"); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if tr.count() != 1 {
		t.Errorf("a retried delivery sent %d times, want 1", tr.count())
	}

	// A different message in the same conversation is a different act.
	if err := s.Deliver(context.Background(), convID, "3 options", "msg-2"); err != nil {
		t.Fatalf("second message: %v", err)
	}
	if tr.count() != 2 {
		t.Errorf("a distinct message must still send, got %d", tr.count())
	}
}

// A journal that cannot answer must not silently become a duplicate sender.
// Refusing is the safe direction: late beats twice.
func TestDeliver_JournalFailureRefusesRatherThanDuplicating(t *testing.T) {
	tr := &fakeTransport{}
	j := newFakeJournal()
	j.markErr = errors.New("journal unavailable")
	s := NewDeliveryService(bound(), registered(), tr)
	s.SetJournal(j)

	if err := s.Deliver(context.Background(), convID, "hello", "msg-1"); err == nil {
		t.Fatal("a failed idempotency check must surface as an error")
	}
	if tr.count() != 0 {
		t.Error("nothing may be sent when idempotency cannot be established")
	}
}

// Permanent failures are dead-lettered and not retried; that is the whole reason
// the distinction exists. A message that failed after its turn already succeeded
// has nowhere else to surface.
func TestDeliver_PermanentFailureIsDeadLettered(t *testing.T) {
	tr := &fakeTransport{err: domain.PermanentDelivery(errors.New("403 bot was blocked by the user"))}
	j := newFakeJournal()
	s := NewDeliveryService(bound(), registered(), tr)
	s.SetJournal(j)

	err := s.Deliver(context.Background(), convID, "hello", "msg-1")
	if !errors.Is(err, domain.ErrDeliveryPermanent) {
		t.Fatalf("expected a permanent error, got %v", err)
	}
	if len(j.dead) != 1 {
		t.Fatalf("expected 1 dead letter, got %d", len(j.dead))
	}
	if j.dead[0].ConversationID != convID || j.dead[0].TxnID != "msg-1" {
		t.Errorf("dead letter lost its provenance: %+v", j.dead[0])
	}
	if j.dead[0].Address.ExternalID != "tg:12345" {
		t.Errorf("dead letter must record where it was headed: %+v", j.dead[0].Address)
	}
}

// A transient failure is NOT dead-lettered — the caller may retry, and recording
// it as terminal would hide a message that would have gone through.
func TestDeliver_TransientFailureIsNotDeadLettered(t *testing.T) {
	tr := &fakeTransport{err: errors.New("429 too many requests")}
	j := newFakeJournal()
	s := NewDeliveryService(bound(), registered(), tr)
	s.SetJournal(j)

	err := s.Deliver(context.Background(), convID, "hello", "msg-1")
	if err == nil || errors.Is(err, domain.ErrDeliveryPermanent) {
		t.Fatalf("expected a transient error, got %v", err)
	}
	if len(j.dead) != 0 {
		t.Errorf("a transient failure must not be dead-lettered, got %d", len(j.dead))
	}
}

// Without a journal delivery still works — best-effort is a valid deployment,
// and the report says so rather than pretending otherwise.
func TestDeliver_WorksWithoutAJournal(t *testing.T) {
	tr := &fakeTransport{}
	s := NewDeliveryService(bound(), registered(), tr)

	if err := s.Deliver(context.Background(), convID, "hello", "msg-1"); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if tr.count() != 1 {
		t.Errorf("expected the message to be sent, got %d", tr.count())
	}
	// Retries duplicate without a journal. Stated here so the trade-off is visible
	// in the tests rather than discovered in production.
	_ = s.Deliver(context.Background(), convID, "hello", "msg-1")
	if tr.count() != 2 {
		t.Errorf("without a journal a retry duplicates; got %d", tr.count())
	}
}

func TestDeliver_RejectsEmptyText(t *testing.T) {
	tr := &fakeTransport{}
	s := NewDeliveryService(bound(), registered(), tr)
	if err := s.Deliver(context.Background(), convID, "", "msg-1"); err == nil {
		t.Error("an empty message would reach the far side as a blank line")
	}
}

func TestDeliver_UnknownConversation(t *testing.T) {
	tr := &fakeTransport{}
	s := NewDeliveryService(fakeConvs{}, registered(), tr)
	if err := s.Deliver(context.Background(), "nope", "hi", "m1"); !errors.Is(err, domain.ErrConversationNotFound) {
		t.Errorf("expected ErrConversationNotFound, got %v", err)
	}
}

// Two agents speaking into the same conversation produce two deliveries, in the
// order they were produced. Nothing here correlates them into a round trip.
func TestDeliver_ManyMessagesOneConversation(t *testing.T) {
	tr := &fakeTransport{}
	s := NewDeliveryService(bound(), registered(), tr)
	s.SetJournal(newFakeJournal())

	for _, m := range []struct{ text, id string }{
		{"Checking now...", "msg-1"},
		{"3 options, from 89 EUR", "msg-2"},
	} {
		if err := s.Deliver(context.Background(), convID, m.text, m.id); err != nil {
			t.Fatalf("%s: %v", m.id, err)
		}
	}
	if tr.count() != 2 {
		t.Fatalf("expected 2 deliveries, got %d", tr.count())
	}
	if tr.sent[0].Text != "Checking now..." || tr.sent[1].Text != "3 options, from 89 EUR" {
		t.Errorf("order was not preserved: %+v", tr.sent)
	}
}
