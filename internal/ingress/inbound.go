package ingress

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/cambrian-sh/core/domain"
)

// ConversationBinder is the slice of the conversation store the inbound path
// needs: find an existing conversation for a sender, or open one and bind it.
type ConversationBinder interface {
	FindByDelivery(ctx context.Context, addr domain.DeliveryAddress) (*domain.Conversation, error)
	CreateConversation(ctx context.Context, c domain.Conversation) error
}

// TurnRunner runs one conversational turn. Satisfied by chat.TurnService.
//
// Narrow on purpose: the inbound path decides WHICH conversation a message
// belongs to and nothing else. What a turn does — history, the worker pool, the
// LLM lease — is the chat tier's business.
type TurnRunner interface {
	RunTurn(ctx context.Context, conversationID, text, clientID string) error
}

// InboundService turns a message from a registered ingress into a turn in the
// right conversation.
//
// It is the inbound counterpart of DeliveryService, and it exists for the same
// reason: the decision "which conversation is this?" must happen in exactly one
// place, server-side, from facts the ingress cannot restate.
//
// Critically, this path does NOT reach the planner. ADR-0080 D4 exists because a
// conversational turn decomposed into plan steps produced "ask the customer to
// provide their booking reference" as an executable step, failed, and emitted the
// failure as spoken dialogue. An ingress message is a conversational turn, so it
// goes to the chat lane the same way a console turn does.
type InboundService struct {
	convs     ConversationBinder
	ingresses domain.IngressResolver
	turns     TurnRunner
	logger    *slog.Logger
	newID     func() string
	// Profile is stamped on conversations this path opens. Customer is the honest
	// default: a message arriving through an outsider-facing entry point is not an
	// employee's, whatever the sender claims.
	Profile domain.ConversationProfile
}

// NewInboundService wires the inbound path.
func NewInboundService(convs ConversationBinder, ingresses domain.IngressResolver, turns TurnRunner) *InboundService {
	return &InboundService{
		convs:     convs,
		ingresses: ingresses,
		turns:     turns,
		logger:    slog.Default(),
		newID:     newConversationID,
		Profile:   domain.ProfileCustomer,
	}
}

// SetLogger overrides the default logger.
func (s *InboundService) SetLogger(l *slog.Logger) {
	if l != nil {
		s.logger = l
	}
}

// InboundMessage is one message arriving through an ingress.
//
// A struct rather than positional arguments because the inbound surface is where
// new facts about a sender will accumulate, and each of them has to be checked
// before it is trusted — an argument list that grows silently is how an unchecked
// field slips in.
type InboundMessage struct {
	// Sender is the ingress daemon's principal, established by the kernel from the
	// connection. NOT a claim in the payload.
	Sender domain.PrincipalRef
	// ExternalID identifies the person on the far side. A claim, checked against
	// the ingress's namespace before it is used for anything.
	ExternalID string
	Text       string
	// Policy is the standing instruction for this conversation, applied ONLY when
	// the conversation is opened. Ignored on every later message.
	Policy string
}

// ErrNotAnIngress is returned when the sender is not a registered entry point.
// The caller should fall through to its ordinary signal handling rather than
// treat this as a failure — most signals are not ingress traffic.
var ErrNotAnIngress = errors.New("ingress: sender is not a registered ingress")

// Accept handles one inbound message.
//
// The order is the design: authorise the SENDER first, then find or open the
// conversation, then run the turn. A sender that may not speak for this external
// identity never reaches the point of creating a conversation, so a rejected
// message leaves no trace to clean up.
func (s *InboundService) Accept(ctx context.Context, m InboundMessage) error {
	sender, externalID, text := m.Sender, m.ExternalID, m.Text
	if s.ingresses == nil {
		return ErrNotAnIngress
	}
	reg, ok := s.ingresses.ResolveIngress(ctx, sender)
	if !ok || reg.IsZero() {
		return ErrNotAnIngress
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("ingress: inbound message has no text")
	}

	// The namespace bound: this ingress may only speak for identities it was
	// registered to carry, so a compromised bridge cannot inject messages as
	// another ingress's users.
	addr, err := reg.DeliveryFor(externalID)
	if err != nil {
		return err
	}

	conv, err := s.resolveConversation(ctx, addr, m.Policy)
	if err != nil {
		return err
	}

	// The message id doubles as the turn's idempotency key, so a redelivered
	// inbound message does not run the turn twice.
	return s.turns.RunTurn(ctx, conv.ID, text, "")
}

// resolveConversation finds this sender's conversation, opening one on first
// contact with its delivery address bound in the same write.
func (s *InboundService) resolveConversation(ctx context.Context, addr domain.DeliveryAddress, policy string) (*domain.Conversation, error) {
	conv, err := s.convs.FindByDelivery(ctx, addr)
	if err == nil && conv != nil {
		if conv.Status != domain.ConversationOpen {
			// A closed conversation is not reopened silently: the sender starts a new
			// one, so a transcript that was ended stays ended.
			return s.open(ctx, addr, policy)
		}
		return conv, nil
	}
	if err != nil && !errors.Is(err, domain.ErrConversationNotFound) {
		return nil, err
	}
	return s.open(ctx, addr, policy)
}

func (s *InboundService) open(ctx context.Context, addr domain.DeliveryAddress, policy string) (*domain.Conversation, error) {
	conv := domain.Conversation{
		ID: s.newID(),
		// Owner is derived from the ingress AND the external identity, so two people
		// on the same ingress do not share a conversation owner. It is deliberately
		// NOT presented as an authenticated identity — the external id is unverified
		// until the linking table exists (ADR-0090 D4), so this is a pseudonym that
		// isolates, not a principal that proves anything.
		OwnerID:  "ingress:" + addr.IngressAgentID + ":" + addr.ExternalID,
		Status:   domain.ConversationOpen,
		Profile:  s.Profile,
		Delivery: addr,
		// Set once, at open (ADR-0084). A later message cannot rewrite the standing
		// instructions of a conversation already in progress — that would let an
		// ingress change the rules mid-transcript.
		Policy: policy,
	}
	if err := s.convs.CreateConversation(ctx, conv); err != nil {
		return nil, fmt.Errorf("ingress: opening a conversation for %s: %w", addr, err)
	}
	s.logger.Info("ADR-0090: conversation opened from ingress",
		"conversation", conv.ID, "address", addr.String())
	return &conv, nil
}

// newConversationID is deliberately unexported and swappable in tests.
func newConversationID() string { return "conv-" + randomHex(12) }

// randomHex returns n bytes of randomness as hex, for conversation ids.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// A conversation id that is not unique would merge two people's transcripts,
		// so this must not fall back to something predictable.
		panic("ingress: no entropy for a conversation id: " + err.Error())
	}
	return hex.EncodeToString(b)
}
