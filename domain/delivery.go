package domain

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Outbound delivery (ADR-0090 D8) — the second of the two one-way flows.
//
// Inbound ends the moment the kernel accepts a signal. Outbound is a separate
// flow that may happen never, once, or many times, seconds or hours later. They
// are not two halves of a round trip, and nothing here waits for anything: an
// agent that says three things produces three deliveries.
//
// The recipient is NEVER carried by the caller. A caller names a conversation and
// the kernel resolves the address from what was bound at first contact (D7).
// ─────────────────────────────────────────────────────────────────────────────

// IngressDelivery is one outbound message handed to an ingress.
type IngressDelivery struct {
	// ConversationID is what the caller named. Carried through so the ingress can
	// log and correlate, never so it can choose a recipient.
	ConversationID string
	// Address is the resolved envelope. The kernel fills this in; a caller cannot.
	Address DeliveryAddress
	// Text is the message to send.
	Text string
	// TxnID makes a redelivery idempotent, following Matrix's transaction ids. The
	// message id is the natural value: delivering message M is the same act however
	// many times it is retried.
	TxnID string
}

// IngressTransport hands a delivery to the ingress daemon that carries it.
//
// Implementations are expected to be fire-and-forget from the kernel's side: the
// call returns once the ingress has accepted the message, NOT once the far side
// has read it. There is no reply value, because a reply is a separate inbound
// flow and correlating them would rebuild the round trip this design removes.
type IngressTransport interface {
	Deliver(ctx context.Context, d IngressDelivery) error
}

// ErrDeliveryPermanent marks a failure that will never succeed on retry — the
// user blocked the bot, the account is gone, the chat was deleted.
//
// The distinction is load-bearing rather than cosmetic: retrying a permanent
// failure forever is how an integration gets rate-limited and then banned, and
// treating a transient outage as permanent silently drops messages that would
// have gone through a minute later.
var ErrDeliveryPermanent = errors.New("delivery: permanently undeliverable")

// ErrNoDeliveryAddress is returned for a conversation nothing can be pushed to —
// a console-only conversation, or one whose ingress never bound an address.
var ErrNoDeliveryAddress = errors.New("delivery: conversation has no delivery address")

// ErrIngressRevoked is returned when the ingress that carried a conversation is
// no longer registered, or no longer permitted to speak for that recipient.
//
// Checked at DELIVERY time, not only at bind time. Revoking a compromised ingress
// has to stop messages that were already addressed through it, otherwise
// revocation only prevents new conversations and the compromised path keeps
// working for every existing one.
var ErrIngressRevoked = errors.New("delivery: the ingress for this conversation is no longer registered")

// PermanentDelivery wraps err so the delivery path treats it as terminal. An
// ingress uses this to say "do not retry" without inventing its own vocabulary.
func PermanentDelivery(err error) error {
	if err == nil {
		return ErrDeliveryPermanent
	}
	return fmt.Errorf("%w: %v", ErrDeliveryPermanent, err)
}

// UndeliverableDelivery records a delivery that will not be retried, so a message
// that failed after its turn already succeeded is visible somewhere rather than
// silently lost. This is the honest cost of going asynchronous: a synchronous
// design would have returned the error to the caller.
type UndeliverableDelivery struct {
	ConversationID string
	Address        DeliveryAddress
	TxnID          string
	Reason         string
	At             time.Time
}

// DeliveryJournal is the durable half of outbound delivery. nil is valid and
// means best-effort: deliveries are attempted, retries may duplicate, and an
// undeliverable message is logged rather than recorded.
type DeliveryJournal interface {
	// MarkDeliveredOnce returns true only the first time key is seen, so a retried
	// delivery does not send twice. Mirrors the reactive journal's exactly-once
	// primitive rather than inventing a second one.
	MarkDeliveredOnce(key string) (firstTime bool, err error)
	// RecordUndeliverable persists a delivery that will not be retried.
	RecordUndeliverable(d UndeliverableDelivery) error
}
