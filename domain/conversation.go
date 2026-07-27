package domain

import (
	"context"
	"errors"
	"strings"
	"time"
)

// ADR-0084 D1: Conversation and Message are first-class kernel entities.
//
// They exist because the kernel previously had NO message model at all: domain.Session
// described itself as "a persistent conversation container" but is a TASK container (Goal,
// Status, plan executions), and the premium chat manager compensated with an in-memory
// transcript that died on restart. Chat state now lives in the kernel, in OSS, so both the
// OSS chat lane and a premium manager read and write the same durable store.
//
// A Conversation is NOT a Session. A Session is goal-scoped work that completes; a
// Conversation is a long-lived, ordered, owned exchange. A turn that actually orders work
// spawns a Session — a 1:N reference, never an identity (ADR-0084 D2).

// ConversationStatus is the lifecycle state of a Conversation.
type ConversationStatus string

const (
	// ConversationOpen accepts new messages.
	ConversationOpen ConversationStatus = "open"
	// ConversationClosed rejects new messages; history remains readable.
	ConversationClosed ConversationStatus = "closed"
)

// ConversationProfile is the recall + scope posture of a conversation (ADR-0084 D7).
//
// This is deliberately a property of the CONVERSATION rather than an agent default. The
// session agent ships `seed_recall = False` ("ground on tools, not shared LTM"), which is
// correct for customer chat and wrong for an employee knowledge assistant — a global default
// someone flips is exactly the kind of setting that silently leaks memory across audiences.
type ConversationProfile string

const (
	// ProfileOperator is the owner/operator talking to their own kernel: full recall, no
	// caller_scope narrowing.
	ProfileOperator ConversationProfile = "operator"
	// ProfileEmployee is an internal user: recall on, narrowed by a caller_scope the
	// manager supplies at conversation open.
	ProfileEmployee ConversationProfile = "employee"
	// ProfileCustomer is an external end user: NO shared-memory recall — grounded on tools
	// only — plus a narrowing caller_scope.
	ProfileCustomer ConversationProfile = "customer"
)

// Recall reports whether this profile may read shared long-term memory. Customer-facing
// conversations are tools-only by construction, so an external user cannot pull on the
// tenant's shared knowledge even if an agent would otherwise recall.
func (p ConversationProfile) Recall() bool { return p != ProfileCustomer }

// Valid reports whether the profile is one of the known postures. An unknown profile is a
// configuration error, never a silent default — the fail-closed discipline.
func (p ConversationProfile) Valid() bool {
	switch p {
	case ProfileOperator, ProfileEmployee, ProfileCustomer:
		return true
	}
	return false
}

// MessageRole identifies who produced a message.
type MessageRole string

const (
	MessageRoleUser   MessageRole = "user"
	MessageRoleAgent  MessageRole = "agent"
	MessageRoleSystem MessageRole = "system"
)

// Valid reports whether the role is known.
func (r MessageRole) Valid() bool {
	switch r {
	case MessageRoleUser, MessageRoleAgent, MessageRoleSystem:
		return true
	}
	return false
}

// Conversation is a durable, owned, ordered exchange.
type Conversation struct {
	ID string
	// OwnerID is the principal that owns this conversation. Within a single kernel,
	// employees and end customers share a store, so ownership is load-bearing security,
	// not a convenience — per-tenant deployment does not remove this need (ADR-0084 D8).
	OwnerID string
	Title   string
	Status  ConversationStatus
	Profile ConversationProfile
	// Policy is an optional system/policy prompt threaded into every turn of this
	// conversation. Set once at open; the turn service reads it from here so a caller does
	// not resend it each turn. Empty means no policy prompt.
	Policy string

	// Delivery is where a reply to this conversation goes: which ingress, and which
	// identity on the far side of it (ADR-0090). Bound by the KERNEL on first
	// inbound contact and never supplied by an agent.
	//
	// This is the envelope, in SMTP's sense: the recipient is data the server owns,
	// not content the sender writes. An agent names a CONVERSATION and the kernel
	// resolves the address — otherwise anything that can produce a message could
	// choose who reads it, and a fire-and-forget ingress would dutifully deliver.
	//
	// Zero means nothing has ever arrived through an ingress, so there is nowhere
	// to deliver. That is the correct state for a console-only conversation, and it
	// happens to match Telegram's own rule that a bot cannot open a chat with
	// someone who never contacted it.
	Delivery DeliveryAddress

	CreatedAt time.Time
	UpdatedAt time.Time
}

// DeliveryAddress is where outbound messages for a conversation are sent.
type DeliveryAddress struct {
	// IngressAgentID is the registered ingress that carries the traffic. It is an
	// agent id, so the kernel can look up the registration and re-check the
	// namespace at delivery time rather than trusting what was stored.
	IngressAgentID string
	// ExternalID identifies the recipient on the far side — a Telegram chat id, a
	// websocket connection key. Opaque to the kernel: only the ingress interprets it.
	ExternalID string
}

// IsZero reports whether no delivery address has been bound.
func (d DeliveryAddress) IsZero() bool { return d.IngressAgentID == "" || d.ExternalID == "" }

// String renders "ingress:external" for logs and audit records.
func (d DeliveryAddress) String() string {
	if d.IsZero() {
		return "<undeliverable>"
	}
	return d.IngressAgentID + ":" + d.ExternalID
}

// Validate checks the invariants the store relies on.
func (c Conversation) Validate() error {
	if strings.TrimSpace(c.ID) == "" {
		return errors.New("conversation: ID is required")
	}
	if strings.TrimSpace(c.OwnerID) == "" {
		return errors.New("conversation: OwnerID is required (ownership is load-bearing security)")
	}
	if c.Status != ConversationOpen && c.Status != ConversationClosed {
		return errors.New("conversation: Status must be open or closed")
	}
	if !c.Profile.Valid() {
		return errors.New("conversation: Profile must be operator, employee, or customer")
	}
	return nil
}

// Message is one turn in a Conversation. Seq is assigned by the store, never the caller.
type Message struct {
	ID             string
	ConversationID string
	// Seq is the monotonic per-conversation ordering key, assigned server-side so
	// concurrent appends cannot collide or reorder.
	Seq     int64
	Role    MessageRole
	Content string
	// ClientID is an OPTIONAL caller-supplied idempotency key. Chat clients retry, and a
	// retried turn must not duplicate a message; when set, appending the same ClientID to
	// the same conversation returns the message already stored instead of writing a second.
	// Mirrors the operator plane's command_id discipline (ADR-0047).
	ClientID  string
	CreatedAt time.Time
}

// Validate checks the invariants the store relies on. Seq is intentionally not validated:
// it is assigned by the store.
func (m Message) Validate() error {
	if strings.TrimSpace(m.ID) == "" {
		return errors.New("message: ID is required")
	}
	if strings.TrimSpace(m.ConversationID) == "" {
		return errors.New("message: ConversationID is required")
	}
	if !m.Role.Valid() {
		return errors.New("message: Role must be user, agent, or system")
	}
	if m.Content == "" {
		return errors.New("message: Content is required")
	}
	return nil
}

// Conversation store errors. Callers distinguish these to return the right status to a
// client — a closed conversation is a client error, a missing one is a not-found.
var (
	ErrConversationNotFound = errors.New("conversation not found")
	ErrConversationClosed   = errors.New("conversation is closed")
)

// ConversationStore is the persistence port for conversations and their messages
// (ADR-0084 D1). The kernel owns this store so conversation state survives a restart of
// whatever is driving the chat — in particular a premium manager running as a supervised
// daemon agent, which cannot own durable state of its own (ADR-0084 D6).
type ConversationStore interface {
	// CreateConversation persists a new conversation.
	CreateConversation(ctx context.Context, c Conversation) error
	// GetConversation returns a conversation, or ErrConversationNotFound.
	GetConversation(ctx context.Context, id string) (*Conversation, error)
	// ListConversations returns an owner's conversations, most recently updated first.
	// A blank ownerID lists across owners (operator/administrative use only).
	ListConversations(ctx context.Context, ownerID string, limit int) ([]Conversation, error)
	// SetConversationStatus opens or closes a conversation.
	SetConversationStatus(ctx context.Context, id string, status ConversationStatus) error

	// AppendMessage assigns the next Seq and stores the message, returning the stored
	// value. Appending to a closed conversation returns ErrConversationClosed; appending
	// to a missing one returns ErrConversationNotFound. When Message.ClientID is set and
	// already present on this conversation, the existing message is returned unchanged.
	AppendMessage(ctx context.Context, m Message) (Message, error)
	// ListMessages returns messages with Seq strictly greater than afterSeq, in order.
	// Pass afterSeq=0 for the whole history. limit <= 0 means no limit.
	ListMessages(ctx context.Context, conversationID string, afterSeq int64, limit int) ([]Message, error)

	// BindDelivery records where replies to this conversation go (ADR-0090).
	//
	// WRITE-ONCE: an already-bound conversation keeps its address and this returns
	// ErrDeliveryAlreadyBound. Rebinding would be a redirect — the one operation an
	// attacker who reached this call would want, since it silently retargets every
	// future reply. Changing a recipient is therefore a deliberate administrative
	// act, not a side effect of a message arriving.
	BindDelivery(ctx context.Context, conversationID string, addr DeliveryAddress) error

	// FindByDelivery returns the conversation bound to addr, if any.
	//
	// This is the inbound hot path: every message arriving through an ingress asks
	// "is this sender already talking to us?". Migration 0006 adds the partial index
	// it reads, partial because in a console-only deployment almost every row is
	// unbound and would otherwise bloat it.
	FindByDelivery(ctx context.Context, addr DeliveryAddress) (*Conversation, error)
}

// ErrDeliveryAlreadyBound is returned when a conversation's delivery address is
// already set. Binding is write-once precisely so a later inbound cannot redirect
// where earlier replies were going.
var ErrDeliveryAlreadyBound = errors.New("conversation: delivery address is already bound")

// ErrDeliveryAddressInvalid is returned for an address that could never deliver.
var ErrDeliveryAddressInvalid = errors.New("conversation: delivery address requires an ingress and an external id")
