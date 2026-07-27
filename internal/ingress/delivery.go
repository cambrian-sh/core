// Package ingress owns the outbound half of ADR-0090: turning "say this in
// conversation C" into a message on the wire of whichever entry point C arrived
// through.
//
// The inbound half needs no package — a signal from an ingress travels the
// ordinary agent plane, and what makes it an ingress signal is the registration
// the kernel looks up (ADR-0090 D2/D3). Outbound needs one because the address
// resolution, the revocation re-check and the permanent/transient split all have
// to happen in exactly one place.
package ingress

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ConversationReader is the narrow slice of the conversation store this needs.
// Deliberately not the whole ConversationStore: delivery reads an address, it
// does not author conversations.
type ConversationReader interface {
	GetConversation(ctx context.Context, id string) (*domain.Conversation, error)
}

// DeliveryService resolves and dispatches outbound messages.
//
// It is the only place that turns a conversation id into a recipient. Everything
// upstream — an agent speaking, a watch firing, an operator broadcasting — names
// a conversation and gets the same resolution, the same revocation check and the
// same failure classification.
type DeliveryService struct {
	convs     ConversationReader
	ingresses domain.IngressResolver
	transport domain.IngressTransport
	journal   domain.DeliveryJournal
	logger    *slog.Logger
	now       func() time.Time
}

// NewDeliveryService wires the outbound path. A nil journal means best-effort
// delivery: still attempted, but a retry may duplicate and an undeliverable
// message is logged rather than recorded.
func NewDeliveryService(convs ConversationReader, ingresses domain.IngressResolver, transport domain.IngressTransport) *DeliveryService {
	return &DeliveryService{
		convs:     convs,
		ingresses: ingresses,
		transport: transport,
		logger:    slog.Default(),
		now:       time.Now,
	}
}

// SetJournal attaches durable idempotency and dead-lettering.
func (s *DeliveryService) SetJournal(j domain.DeliveryJournal) { s.journal = j }

// SetLogger overrides the default logger.
func (s *DeliveryService) SetLogger(l *slog.Logger) {
	if l != nil {
		s.logger = l
	}
}

// Deliver sends text to whoever the conversation belongs to.
//
// txnID makes a retry idempotent; pass the message id, since delivering message M
// is the same act however many times it is attempted. An empty txnID disables the
// idempotency check for this call rather than blocking it — a caller with nothing
// stable to key on should still be able to send.
//
// The order of the checks is the design: resolve, then re-authorise, then send.
// Re-authorising after resolution is what makes revocation effective on
// conversations that were bound while the ingress was still trusted.
func (s *DeliveryService) Deliver(ctx context.Context, conversationID, text, txnID string) error {
	if s.transport == nil {
		return errors.New("delivery: no ingress transport configured")
	}
	if text == "" {
		// An empty delivery would reach the far side as a blank message, which reads
		// as a bug to the person receiving it.
		return errors.New("delivery: text is required")
	}

	conv, err := s.convs.GetConversation(ctx, conversationID)
	if err != nil {
		return err
	}
	if conv.Delivery.IsZero() {
		return fmt.Errorf("%w: %s", domain.ErrNoDeliveryAddress, conversationID)
	}

	// Re-authorise against the CURRENT registration. An address bound yesterday
	// through an ingress that has since been revoked, or whose namespace was
	// narrowed, must not still deliver.
	if err := s.authorise(ctx, conv.Delivery); err != nil {
		return err
	}

	d := domain.IngressDelivery{
		ConversationID: conversationID,
		Address:        conv.Delivery,
		Text:           text,
		TxnID:          txnID,
	}

	// Idempotency before the send, so a retry that already succeeded does not
	// produce a second message on the far side.
	if s.journal != nil && txnID != "" {
		first, jerr := s.journal.MarkDeliveredOnce(deliveryKey(conversationID, txnID))
		if jerr != nil {
			// A journal that cannot answer must not silently become a duplicate
			// sender. Refusing is the safe direction: the caller can retry, and a
			// message arriving late beats one arriving twice.
			return fmt.Errorf("delivery: idempotency check failed: %w", jerr)
		}
		if !first {
			return nil // already delivered on an earlier attempt
		}
	}

	if err := s.transport.Deliver(ctx, d); err != nil {
		return s.classify(d, err)
	}
	return nil
}

// authorise re-checks that the bound ingress is still registered and still
// permitted to speak for this recipient.
func (s *DeliveryService) authorise(ctx context.Context, addr domain.DeliveryAddress) error {
	if s.ingresses == nil {
		// No registry: nothing was ever registered, so nothing can be revoked. This
		// is the OSS shape, where an address could only have been bound by a caller
		// that already had the standing to do it.
		return nil
	}
	reg, ok := s.ingresses.ResolveIngress(ctx, domain.AgentPrincipal(addr.IngressAgentID))
	if !ok || reg.IsZero() {
		return fmt.Errorf("%w: %s", domain.ErrIngressRevoked, addr.IngressAgentID)
	}
	if !reg.MaySpeakFor(addr.ExternalID) {
		// The namespace was narrowed after this address was bound.
		return fmt.Errorf("%w: %s may no longer speak for %s",
			domain.ErrIngressRevoked, addr.IngressAgentID, addr.ExternalID)
	}
	return nil
}

// classify decides whether a failed delivery is worth retrying, and dead-letters
// the ones that are not.
//
// A permanent failure that kept being retried is how an integration gets
// rate-limited and then banned; a transient one treated as permanent silently
// drops a message that would have gone through a minute later. Only the ingress
// knows which it is, so the ingress says so with domain.PermanentDelivery and
// anything unlabelled is assumed transient — the recoverable assumption.
func (s *DeliveryService) classify(d domain.IngressDelivery, err error) error {
	if !errors.Is(err, domain.ErrDeliveryPermanent) {
		return err // transient: the caller may retry
	}

	dl := domain.UndeliverableDelivery{
		ConversationID: d.ConversationID,
		Address:        d.Address,
		TxnID:          d.TxnID,
		Reason:         err.Error(),
		At:             s.now(),
	}
	if s.journal != nil {
		if jerr := s.journal.RecordUndeliverable(dl); jerr != nil {
			s.logger.Error("ADR-0090: undeliverable message could not be recorded",
				"conversation", d.ConversationID, "address", d.Address.String(), "err", jerr)
		}
	} else {
		// Without a journal this is the only trace. A message that failed after its
		// turn already succeeded has nowhere else to surface.
		s.logger.Warn("ADR-0090: permanently undeliverable, and no journal to record it",
			"conversation", d.ConversationID, "address", d.Address.String(), "reason", err)
	}
	return err
}

// deliveryKey namespaces the idempotency key by conversation, so two
// conversations cannot collide on a caller's message id.
func deliveryKey(conversationID, txnID string) string {
	return "delivery:" + conversationID + ":" + txnID
}
