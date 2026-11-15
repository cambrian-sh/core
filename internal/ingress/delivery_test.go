package ingress

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

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

// fakeJournal is an in-memory DeliveryJournal with the real compare-and-set
// semantics (the bbolt one is tested in internal/storage and internal/kernel).
type fakeJournal struct {
	mu      sync.Mutex
	intents map[string]domain.DeliveryIntent
	prepErr error
}

func newFakeJournal() *fakeJournal {
	return &fakeJournal{intents: map[string]domain.DeliveryIntent{}}
}

func (f *fakeJournal) PrepareDelivery(_ context.Context, in domain.DeliveryIntent) (domain.DeliveryIntent, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.prepErr != nil {
		return domain.DeliveryIntent{}, false, f.prepErr
	}
	if got, ok := f.intents[in.Key]; ok {
		return got, false, nil
	}
	in.State = domain.DeliveryPrepared
	in.UpdatedAt = in.CreatedAt
	f.intents[in.Key] = in
	return in, true, nil
}

func (f *fakeJournal) AdvanceDelivery(_ context.Context, key string, from, to domain.DeliveryIntentState, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	in, ok := f.intents[key]
	if !ok {
		return errors.New("unknown key")
	}
	if !domain.CanDeliveryTransition(from, to) {
		return errors.New("illegal transition")
	}
	if in.State != from {
		return domain.ErrDeliveryStateConflict
	}
	in.State, in.Err = to, reason
	if to == domain.DeliveryDispatched {
		in.Attempts++
	}
	if to == domain.DeliveryConfirmed {
		in.Text = ""
	}
	f.intents[key] = in
	return nil
}

func (f *fakeJournal) ListDeliveries(_ context.Context, states []domain.DeliveryIntentState, _ int) ([]domain.DeliveryIntent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []domain.DeliveryIntent
	for _, in := range f.intents {
		for _, st := range states {
			if in.State == st {
				out = append(out, in)
			}
		}
	}
	return out, nil
}

func (f *fakeJournal) PruneDeliveries(context.Context, time.Time, int) (int, bool, error) {
	return 0, false, nil
}

func (f *fakeJournal) get(txnID string) domain.DeliveryIntent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.intents[deliveryKey(convID, txnID)]
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

// A journal that cannot record must not silently become a duplicate (or lost)
// sender. Refusing is the safe direction: late beats twice.
func TestDeliver_JournalFailureRefusesRatherThanDuplicating(t *testing.T) {
	tr := &fakeTransport{}
	j := newFakeJournal()
	j.prepErr = errors.New("journal unavailable")
	s := NewDeliveryService(bound(), registered(), tr)
	s.SetJournal(j)

	if err := s.Deliver(context.Background(), convID, "hello", "msg-1"); err == nil {
		t.Fatal("a failed prepare must surface as an error")
	}
	if tr.count() != 0 {
		t.Error("nothing may be sent when the intent cannot be recorded")
	}
}

// Permanent failures settle as failed (the dead letter) and are never retried.
func TestDeliver_PermanentFailureIsDeadLettered(t *testing.T) {
	tr := &fakeTransport{err: domain.PermanentDelivery(errors.New("403 bot was blocked by the user"))}
	j := newFakeJournal()
	s := NewDeliveryService(bound(), registered(), tr)
	s.SetJournal(j)

	err := s.Deliver(context.Background(), convID, "hello", "msg-1")
	if !errors.Is(err, domain.ErrDeliveryPermanent) {
		t.Fatalf("expected a permanent error, got %v", err)
	}
	in := j.get("msg-1")
	if in.State != domain.DeliveryFailed {
		t.Fatalf("intent state = %s, want failed", in.State)
	}
	if in.ConversationID != convID || in.TxnID != "msg-1" || in.Address.ExternalID != "tg:12345" {
		t.Errorf("dead letter lost its provenance: %+v", in)
	}
	// A retry returns the recorded failure without sending again.
	tr.err = nil
	if err := s.Deliver(context.Background(), convID, "hello", "msg-1"); !errors.Is(err, domain.ErrDeliveryPermanent) {
		t.Fatalf("retry of a failed intent: %v", err)
	}
	if tr.count() != 0 {
		t.Error("a failed intent must not be resent")
	}
}

// ENG-1172's root cause: the old journal marked a message delivered BEFORE the
// send, so a transient failure followed by a retry returned success without
// sending. Two-phase: the transient failure puts the intent back to prepared and
// the retry sends it — exactly once.
func TestDeliver_TransientFailureThenRetrySendsExactlyOnce(t *testing.T) {
	tr := &fakeTransport{err: domain.TransientDelivery(errors.New("no live daemon"))}
	j := newFakeJournal()
	s := NewDeliveryService(bound(), registered(), tr)
	s.SetJournal(j)

	err := s.Deliver(context.Background(), convID, "hello", "msg-1")
	if !errors.Is(err, domain.ErrDeliveryTransient) {
		t.Fatalf("expected a transient error, got %v", err)
	}
	if st := j.get("msg-1").State; st != domain.DeliveryPrepared {
		t.Fatalf("after a transient failure the intent is %s, want prepared", st)
	}
	tr.err = nil
	for i := 0; i < 2; i++ {
		if err := s.Deliver(context.Background(), convID, "hello", "msg-1"); err != nil {
			t.Fatalf("retry %d: %v", i, err)
		}
	}
	if tr.count() != 1 {
		t.Fatalf("sent %d times, want exactly 1 (no drop, no duplicate)", tr.count())
	}
	in := j.get("msg-1")
	if in.State != domain.DeliveryConfirmed || in.Text != "" {
		t.Errorf("confirmed intent = %+v; want confirmed with the text dropped", in)
	}
}

// An unlabelled failure after dispatch may have reached the far side: it is
// recorded as ambiguous and never resent automatically.
func TestDeliver_UnlabelledFailureIsAmbiguousAndNotResent(t *testing.T) {
	tr := &fakeTransport{err: errors.New("context deadline exceeded")}
	j := newFakeJournal()
	s := NewDeliveryService(bound(), registered(), tr)
	s.SetJournal(j)

	if err := s.Deliver(context.Background(), convID, "hello", "msg-1"); !errors.Is(err, domain.ErrDeliveryAmbiguous) {
		t.Fatalf("expected ErrDeliveryAmbiguous, got %v", err)
	}
	if st := j.get("msg-1").State; st != domain.DeliveryAmbiguous {
		t.Fatalf("state = %s, want ambiguous", st)
	}
	tr.err = nil
	if err := s.Deliver(context.Background(), convID, "hello", "msg-1"); !errors.Is(err, domain.ErrDeliveryAmbiguous) {
		t.Fatalf("retry of an ambiguous intent: %v", err)
	}
	s.RedriveGrace = 0
	s.Redrive(context.Background())
	if tr.count() != 0 {
		t.Error("an ambiguous intent must not be resent by a retry or the redriver")
	}
}

// A crash between prepare and dispatch leaves a prepared intent nobody retries;
// the redriver sends it once its grace period has passed.
func TestRedrive_SendsAPreparedIntentLeftByACrash(t *testing.T) {
	tr := &fakeTransport{}
	j := newFakeJournal()
	s := NewDeliveryService(bound(), registered(), tr)
	s.SetJournal(j)
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	_, _, _ = j.PrepareDelivery(context.Background(), domain.DeliveryIntent{
		Key: deliveryKey(convID, "msg-9"), ConversationID: convID, TxnID: "msg-9",
		Address: bound()[convID].Delivery, Text: "the answer", CreatedAt: now,
	})
	s.Redrive(context.Background()) // inside the grace period: left to its caller
	if tr.count() != 0 {
		t.Fatal("an intent inside the grace period must be left to its caller")
	}
	now = now.Add(DefaultRedriveGrace + time.Second)
	s.Redrive(context.Background())
	if tr.count() != 1 || tr.sent[0].Text != "the answer" || tr.sent[0].TxnID != "msg-9" {
		t.Fatalf("redrive sent %+v", tr.sent)
	}
	if st := j.get("msg-9").State; st != domain.DeliveryConfirmed {
		t.Errorf("state = %s, want confirmed", st)
	}
}

// A process that died mid-send leaves a dispatched intent: the redriver marks it
// ambiguous rather than resending it.
func TestRedrive_StaleDispatchedBecomesAmbiguous(t *testing.T) {
	tr := &fakeTransport{}
	j := newFakeJournal()
	s := NewDeliveryService(bound(), registered(), tr)
	s.SetJournal(j)
	s.RedriveGrace = 0
	key := deliveryKey(convID, "msg-3")
	_, _, _ = j.PrepareDelivery(context.Background(), domain.DeliveryIntent{
		Key: key, ConversationID: convID, TxnID: "msg-3", Address: bound()[convID].Delivery, Text: "x",
	})
	_ = j.AdvanceDelivery(context.Background(), key, domain.DeliveryPrepared, domain.DeliveryDispatched, "")

	s.Redrive(context.Background())
	if st := j.get("msg-3").State; st != domain.DeliveryAmbiguous {
		t.Fatalf("state = %s, want ambiguous", st)
	}
	if tr.count() != 0 {
		t.Error("a possibly-sent intent must not be resent")
	}
}

// Retries are bounded: past MaxAttempts a prepared intent fails (visible), and an
// intent whose ingress was revoked fails instead of being sent.
func TestRedrive_FailsExhaustedAndRevokedIntents(t *testing.T) {
	tr := &fakeTransport{}
	j := newFakeJournal()
	s := NewDeliveryService(bound(), fakeIngresses{}, tr) // ingress revoked
	s.SetJournal(j)
	s.RedriveGrace = 0
	s.MaxAttempts = 2
	addr := bound()[convID].Delivery

	exhausted := deliveryKey(convID, "msg-a")
	_, _, _ = j.PrepareDelivery(context.Background(), domain.DeliveryIntent{Key: exhausted, ConversationID: convID, TxnID: "msg-a", Address: addr, Text: "x"})
	for i := 0; i < 2; i++ {
		_ = j.AdvanceDelivery(context.Background(), exhausted, domain.DeliveryPrepared, domain.DeliveryDispatched, "")
		_ = j.AdvanceDelivery(context.Background(), exhausted, domain.DeliveryDispatched, domain.DeliveryPrepared, "transient")
	}
	revoked := deliveryKey(convID, "msg-b")
	_, _, _ = j.PrepareDelivery(context.Background(), domain.DeliveryIntent{Key: revoked, ConversationID: convID, TxnID: "msg-b", Address: addr, Text: "y"})

	s.Redrive(context.Background())
	if st := j.get("msg-a").State; st != domain.DeliveryFailed {
		t.Errorf("exhausted intent = %s, want failed", st)
	}
	if in := j.get("msg-b"); in.State != domain.DeliveryFailed || !strings.Contains(in.Err, "no longer registered") {
		t.Errorf("revoked intent = %+v, want failed with the revocation reason", in)
	}
	if tr.count() != 0 {
		t.Error("neither intent may be sent")
	}
}

func TestDeliveryService_StartStopIsClean(t *testing.T) {
	s := NewDeliveryService(bound(), registered(), &fakeTransport{})
	s.SetJournal(newFakeJournal())
	s.RedriveInterval = time.Millisecond
	s.Start(context.Background())
	time.Sleep(5 * time.Millisecond)
	s.Stop()
	s.Stop() // idempotent
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

// ADR-0098 D6 (owner decision 2026-11-15): progress reaches only an ingress whose
// registration opted in; one that did not gets nothing and no error (an error
// would make the progress bridge back off for nothing). A real reply is
// unaffected by the flag.
func TestDeliverProgress_OnlyToAnIngressThatOptedIn(t *testing.T) {
	tr := &fakeTransport{}
	off := registered()
	s := NewDeliveryService(bound(), off, tr)
	if err := s.DeliverProgress(context.Background(), convID, "still working", false); err != nil {
		t.Fatalf("DeliverProgress to an ingress without progress: %v, want a quiet no-op", err)
	}
	if tr.count() != 0 {
		t.Fatalf("progress was sent to an ingress that did not opt in")
	}
	if err := s.Deliver(context.Background(), convID, "the answer", "msg-1"); err != nil || tr.count() != 1 {
		t.Fatalf("a reply must not depend on the progress flag: err=%v sent=%d", err, tr.count())
	}

	on := registered()
	reg := on["telegram_ingress"]
	reg.Progress = true
	on["telegram_ingress"] = reg
	tr2 := &fakeTransport{}
	if err := NewDeliveryService(bound(), on, tr2).DeliverProgress(context.Background(), convID, "still working", false); err != nil {
		t.Fatalf("DeliverProgress: %v", err)
	}
	if tr2.count() != 1 || tr2.sent[0].Kind != domain.DeliveryKindProgress {
		t.Fatalf("an opted-in ingress must receive the snapshot, got %+v", tr2.sent)
	}

	// A revoked ingress is still refused, flag or not.
	if err := NewDeliveryService(bound(), fakeIngresses{}, &fakeTransport{}).DeliverProgress(context.Background(), convID, "x", false); !errors.Is(err, domain.ErrIngressRevoked) {
		t.Fatalf("revoked ingress: %v, want ErrIngressRevoked", err)
	}
}
