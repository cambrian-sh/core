package ingress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

type fakeCaller struct {
	gotStream  string
	gotHandoff *domain.Handoff
	resp       *domain.Handoff
	err        error
}

func (f *fakeCaller) CallDaemon(_ context.Context, streamID string, h *domain.Handoff) (*domain.Handoff, error) {
	f.gotStream = streamID
	f.gotHandoff = h
	return f.resp, f.err
}

func ack(t *testing.T, v any) *domain.Handoff {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal ack: %v", err)
	}
	return &domain.Handoff{Payload: &domain.Payload{Data: b}}
}

var testDelivery = domain.IngressDelivery{
	ConversationID: "conv-7",
	Address:        domain.DeliveryAddress{IngressAgentID: "telegram_ingress", ExternalID: "tg:12345"},
	Text:           "Which dates?",
	TxnID:          "msg-1",
}

// The daemon receives the recipient and the text; an ingress is ONE long-lived
// daemon, so it is addressed by its agent id rather than per conversation.
func TestDaemonTransport_AddressesTheIngressDaemon(t *testing.T) {
	c := &fakeCaller{}
	if err := NewDaemonTransport(c).Deliver(context.Background(), testDelivery); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if c.gotStream != "telegram_ingress" {
		t.Errorf("stream = %q, want the ingress agent id", c.gotStream)
	}

	var body deliveryPayload
	if err := json.Unmarshal(c.gotHandoff.Payload.Data, &body); err != nil {
		t.Fatalf("payload is not decodable: %v", err)
	}
	if body.Recipient != "tg:12345" || body.Text != "Which dates?" {
		t.Errorf("payload drifted: %+v", body)
	}
	if body.ConversationID != "conv-7" || body.TxnID != "msg-1" {
		t.Errorf("provenance lost: %+v", body)
	}
}

// A deployment running several instances of one ingress needs a different rule,
// and should not have to fork the transport to get it.
func TestDaemonTransport_StreamResolverIsOverridable(t *testing.T) {
	c := &fakeCaller{}
	tr := NewDaemonTransport(c)
	tr.SetStreamResolver(func(id string) string { return "shard-2:" + id })

	if err := tr.Deliver(context.Background(), testDelivery); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if c.gotStream != "shard-2:telegram_ingress" {
		t.Errorf("stream = %q, want the overridden value", c.gotStream)
	}
	// A nil resolver must not wipe the default.
	tr.SetStreamResolver(nil)
	_ = tr.Deliver(context.Background(), testDelivery)
	if c.gotStream != "shard-2:telegram_ingress" {
		t.Errorf("a nil resolver should be ignored, got %q", c.gotStream)
	}
}

// The daemon says whether a failure is terminal, because only it knows. That
// answer decides whether the message is retried or dead-lettered.
func TestDaemonTransport_PermanentAckIsTerminal(t *testing.T) {
	c := &fakeCaller{resp: ack(t, map[string]any{
		"status": "failed", "permanent": true, "error": "403 bot was blocked by the user",
	})}
	err := NewDaemonTransport(c).Deliver(context.Background(), testDelivery)
	if !errors.Is(err, domain.ErrDeliveryPermanent) {
		t.Fatalf("expected a permanent error, got %v", err)
	}
}

func TestDaemonTransport_TransientAckIsRetryable(t *testing.T) {
	c := &fakeCaller{resp: ack(t, map[string]any{
		"status": "failed", "permanent": false, "error": "429 too many requests",
	})}
	err := NewDaemonTransport(c).Deliver(context.Background(), testDelivery)
	if err == nil {
		t.Fatal("a reported failure must surface")
	}
	if errors.Is(err, domain.ErrDeliveryPermanent) {
		t.Error("an unlabelled failure must be treated as transient, not terminal")
	}
	// The daemon REPORTED it did not send: positive evidence, so retryable
	// (ADR-0090 D8.1).
	if !errors.Is(err, domain.ErrDeliveryTransient) {
		t.Errorf("a reported non-permanent failure must be ErrDeliveryTransient, got %v", err)
	}
}

// A rate-limited ingress says how long to wait (a Telegram 429's retry_after).
// The transport carries it on the error so a self-pacing sender — the progress
// bridge — can honour it, and the failure stays transient
// (T-20261113-progress-back-off-never-widens-on-a-telegram, ADR-0098 D4).
func TestDaemonTransport_RetryAfterTravelsOnTheTransientError(t *testing.T) {
	c := &fakeCaller{resp: ack(t, map[string]any{
		"status": "failed", "permanent": false, "error": "429 too many requests", "retry_after": 12.5,
	})}
	err := NewDaemonTransport(c).Deliver(context.Background(), testDelivery)
	if !errors.Is(err, domain.ErrDeliveryTransient) {
		t.Fatalf("a rate-limited ack must stay ErrDeliveryTransient, got %v", err)
	}
	d, ok := domain.DeliveryRetryAfter(err)
	if !ok || d != 12500*time.Millisecond {
		t.Fatalf("DeliveryRetryAfter = (%v, %v), want 12.5s", d, ok)
	}
	// Without retry_after there is nothing to honour.
	c.resp = ack(t, map[string]any{"status": "failed", "error": "boom"})
	if _, ok := domain.DeliveryRetryAfter(NewDaemonTransport(c).Deliver(context.Background(), testDelivery)); ok {
		t.Fatal("an ack with no retry_after must not report one")
	}
}

// No live daemon: the call never started, so nothing was sent — transient, and
// the journal retries it. The daemon may be restarting under supervision.
func TestDaemonTransport_NoLiveDaemonIsTransient(t *testing.T) {
	c := &fakeCaller{err: fmt.Errorf("CallDaemon: %w: no daemon registered", domain.ErrDaemonUnavailable)}
	err := NewDaemonTransport(c).Deliver(context.Background(), testDelivery)
	if !errors.Is(err, domain.ErrDeliveryTransient) {
		t.Fatalf("no live daemon must be ErrDeliveryTransient, got %v", err)
	}
}

// Any other call failure may have happened after the daemon received the
// handoff: it is neither permanent nor proven transient, so the journal records
// it as ambiguous instead of resending (ADR-0090 D8.1).
func TestDaemonTransport_OtherCallFailureIsUnlabelled(t *testing.T) {
	c := &fakeCaller{err: errors.New("rpc error: deadline exceeded")}
	err := NewDaemonTransport(c).Deliver(context.Background(), testDelivery)
	if err == nil {
		t.Fatal("a call failure must surface")
	}
	if errors.Is(err, domain.ErrDeliveryPermanent) || errors.Is(err, domain.ErrDeliveryTransient) {
		t.Errorf("a mid-call failure is ambiguous, not %v", err)
	}
}

// Silence, an empty body, an "ok" status and an unparseable answer all mean
// accepted. Treating any of them as a failure would duplicate the message on the
// far side when the caller retries.
func TestDaemonTransport_AcceptedShapes(t *testing.T) {
	cases := map[string]*domain.Handoff{
		"nil response":  nil,
		"no payload":    {},
		"empty payload": {Payload: &domain.Payload{}},
		"unparseable":   {Payload: &domain.Payload{Data: []byte("sent!")}},
		"status ok":     ack(t, map[string]any{"status": "ok"}),
		"status sent":   ack(t, map[string]any{"status": "sent"}),
	}
	for name, resp := range cases {
		c := &fakeCaller{resp: resp}
		if err := NewDaemonTransport(c).Deliver(context.Background(), testDelivery); err != nil {
			t.Errorf("%s: expected acceptance, got %v", name, err)
		}
	}
}

func TestDaemonTransport_NoCallerConfigured(t *testing.T) {
	if err := NewDaemonTransport(nil).Deliver(context.Background(), testDelivery); err == nil {
		t.Error("delivering with no caller must fail loudly")
	}
}
