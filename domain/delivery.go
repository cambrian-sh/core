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
	// Kind separates a real reply from a supersedable progress snapshot (ADR-0098).
	// Empty means DeliveryKindMessage, so every existing caller keeps its meaning.
	//
	// The distinction has to reach the ingress, because the two are rendered
	// differently: a message is appended, a progress snapshot REPLACES the last one.
	// An ingress that does not understand progress ignores it, which is the correct
	// degradation — silence rather than a wall of status lines.
	Kind DeliveryKind
	// Final marks the terminal progress update of a turn (ADR-0098 D3).
	Final bool
}

// DeliveryKind distinguishes what an ingress is being asked to render.
type DeliveryKind string

const (
	// DeliveryKindMessage is a real conversation turn: append it.
	DeliveryKindMessage DeliveryKind = "ingress.deliver"
	// DeliveryKindProgress is a supersedable snapshot: replace the previous one, and
	// never treat it as conversation content (ADR-0098 D1/D2).
	DeliveryKindProgress DeliveryKind = "ingress.progress"
)

// Resolved returns the effective kind, defaulting to a message.
func (k DeliveryKind) Resolved() DeliveryKind {
	if k == "" {
		return DeliveryKindMessage
	}
	return k
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

// ErrDeliveryTransient marks a failure with POSITIVE evidence that nothing reached
// the far side — no live ingress daemon to hand it to, or the daemon itself said it
// did not send. Only such a failure is retried automatically (ADR-0090 D8.1).
//
// An unlabelled failure after the message may have left the process (a timeout, a
// reset) is NOT transient: absence of a receipt is not evidence of failure, so it is
// recorded as ambiguous instead of being resent (ADR-0114 D18).
var ErrDeliveryTransient = errors.New("delivery: transient failure, nothing was sent")

// ErrDeliveryAmbiguous is returned when an earlier attempt at the same message may
// have reached the far side without a confirmation. The kernel refuses to send it
// again rather than risk a duplicate; the intent stays visible as ambiguous.
var ErrDeliveryAmbiguous = errors.New("delivery: an earlier attempt may have been delivered; not resending")

// Operator resolution of a delivery intent (ENG-1657, owner decision 2026-11-20,
// option A): an AMBIGUOUS intent — it may or may not have reached the far side, and
// the kernel never resends one by itself — is settled by a person who checked.
const (
	// DeliveryResolveConfirm: the message did arrive; ambiguous → confirmed.
	DeliveryResolveConfirm = "confirm"
	// DeliveryResolveResend: send it once more (same txn id, so a far side that
	// dedups on it sees one message), then settle by the outcome.
	DeliveryResolveResend = "resend"
)

var (
	// ErrDeliveryIntentNotFound: no intent with that key (never recorded, or pruned).
	ErrDeliveryIntentNotFound = errors.New("delivery: no such delivery intent")
	// ErrDeliveryIntentNotAmbiguous: only an ambiguous intent can be resolved; a
	// confirmed or failed one is settled, a prepared or dispatched one is the
	// redriver's.
	ErrDeliveryIntentNotAmbiguous = errors.New("delivery: the intent is not ambiguous")
	// ErrDeliveryJournalOff: the kernel runs delivery best-effort, with no journal
	// to list or resolve.
	ErrDeliveryJournalOff = errors.New("delivery: no delivery journal on this kernel")
)

// ErrDeliveryStateConflict is returned by DeliveryJournal.AdvanceDelivery when the
// intent is not in the expected state — another attempt moved it first.
var ErrDeliveryStateConflict = errors.New("delivery: intent is not in the expected state")

// ErrDaemonUnavailable marks a daemon call that could not start because no live
// daemon serves the stream. Nothing was sent, so it is transient evidence.
var ErrDaemonUnavailable = errors.New("daemon: no live daemon for stream")

// TransientDelivery wraps err as a failure where nothing was sent.
func TransientDelivery(err error) error {
	if err == nil {
		return ErrDeliveryTransient
	}
	return fmt.Errorf("%w: %v", ErrDeliveryTransient, err)
}

// DeliveryRetryAfterError is a delivery failure whose far side said how long to
// wait before sending again — a Telegram 429 with parameters.retry_after, an HTTP
// Retry-After. The ingress daemon reports it in its ack (`retry_after`, seconds);
// the progress bridge widens its interval to at least After (ADR-0098 D4,
// T-20261113-progress-back-off-never-widens-on-a-telegram). It wraps the
// underlying error, so errors.Is(err, ErrDeliveryTransient) still holds.
type DeliveryRetryAfterError struct {
	After time.Duration
	Err   error
}

func (e *DeliveryRetryAfterError) Error() string {
	return fmt.Sprintf("%v (retry after %s)", e.Err, e.After)
}

func (e *DeliveryRetryAfterError) Unwrap() error { return e.Err }

// DeliveryRetryAfter reports the wait a delivery failure asked for, if any.
func DeliveryRetryAfter(err error) (time.Duration, bool) {
	var r *DeliveryRetryAfterError
	if errors.As(err, &r) && r.After > 0 {
		return r.After, true
	}
	return 0, false
}

// DeliveryIntentState is a delivery intent's position (ADR-0090 D8.1). The states
// and their meaning are the pipeline effect-intent protocol's (ADR-0114 D19),
// restated here because core never imports premium.
type DeliveryIntentState string

const (
	// DeliveryPrepared: recorded, nothing has left the process — safe to send.
	DeliveryPrepared DeliveryIntentState = "prepared"
	// DeliveryDispatched: the send is in progress or the process died during it.
	DeliveryDispatched DeliveryIntentState = "dispatched"
	// DeliveryConfirmed: the ingress accepted it. Terminal.
	DeliveryConfirmed DeliveryIntentState = "confirmed"
	// DeliveryAmbiguous: it may or may not have reached the far side. Never resent
	// automatically; an operator decides.
	DeliveryAmbiguous DeliveryIntentState = "ambiguous"
	// DeliveryFailed: permanently undeliverable, or retries exhausted. Terminal —
	// this is the dead letter.
	DeliveryFailed DeliveryIntentState = "failed"
)

// legalDeliveryTransitions mirrors pipeline.legalIntentTransitions. dispatched →
// prepared is legal only on positive evidence nothing was sent (ErrDeliveryTransient).
var legalDeliveryTransitions = map[DeliveryIntentState]map[DeliveryIntentState]bool{
	DeliveryPrepared:   {DeliveryDispatched: true, DeliveryFailed: true},
	DeliveryDispatched: {DeliveryConfirmed: true, DeliveryAmbiguous: true, DeliveryFailed: true, DeliveryPrepared: true},
	DeliveryAmbiguous:  {DeliveryConfirmed: true, DeliveryFailed: true},
	DeliveryConfirmed:  {},
	DeliveryFailed:     {},
}

// CanDeliveryTransition reports whether from → to is a legal intent transition.
func CanDeliveryTransition(from, to DeliveryIntentState) bool {
	return legalDeliveryTransitions[from][to]
}

// DeliveryIntent is one outbound message the kernel decided to send. Its key is
// its identity: delivering message M is the same act however often it is tried.
type DeliveryIntent struct {
	Key            string
	ConversationID string
	TxnID          string
	Address        DeliveryAddress
	// Text is kept while the intent can still be (re)sent and dropped once it is
	// confirmed, so the journal does not become a second copy of the conversation.
	Text      string
	State     DeliveryIntentState
	Attempts  int
	Err       string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// DeliveryJournal is the durable half of outbound delivery (ADR-0090 D8.1): a
// two-phase intent record per message. nil is valid and means best-effort: a
// delivery is attempted once, a retry may duplicate, and a failure is only logged.
type DeliveryJournal interface {
	// PrepareDelivery records in as a prepared intent unless its key already
	// exists, and returns the stored intent; created reports whether it was new.
	PrepareDelivery(ctx context.Context, in DeliveryIntent) (stored DeliveryIntent, created bool, err error)
	// AdvanceDelivery moves key from → to (compare-and-set) and records reason.
	// Moving to dispatched counts an attempt; moving to confirmed drops the text.
	// Returns ErrDeliveryStateConflict when the intent is not in state from, and
	// an error for an illegal transition or an unknown key.
	AdvanceDelivery(ctx context.Context, key string, from, to DeliveryIntentState, reason string) error
	// ListDeliveries returns intents in any of states, least recently updated
	// first, at most limit (limit <= 0 ⇒ all).
	ListDeliveries(ctx context.Context, states []DeliveryIntentState, limit int) ([]DeliveryIntent, error)
	// PruneDeliveries removes up to limit confirmed, failed or ambiguous intents
	// last updated before `before`; more reports whether eligible ones remain.
	PruneDeliveries(ctx context.Context, before time.Time, limit int) (removed int, more bool, err error)
}
