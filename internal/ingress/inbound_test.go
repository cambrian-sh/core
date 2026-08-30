package ingress

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

type fakeBinder struct {
	mu      sync.Mutex
	convs   []domain.Conversation
	createN int
	findErr error
}

func (f *fakeBinder) FindByDelivery(_ context.Context, addr domain.DeliveryAddress) (*domain.Conversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.findErr != nil {
		return nil, f.findErr
	}
	for i := range f.convs {
		if f.convs[i].Delivery == addr {
			c := f.convs[i]
			return &c, nil
		}
	}
	return nil, domain.ErrConversationNotFound
}

func (f *fakeBinder) CreateConversation(_ context.Context, c domain.Conversation) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createN++
	f.convs = append(f.convs, c)
	return nil
}

type fakeTurns struct {
	mu  sync.Mutex
	ran []struct{ conv, text string }
	// prins captures the principal each turn's context carried, one per run —
	// the zero PrincipalRef when nothing was stamped.
	prins []domain.PrincipalRef
	err   error
}

func (f *fakeTurns) RunTurn(ctx context.Context, conv, text, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.ran = append(f.ran, struct{ conv, text string }{conv, text})
	f.prins = append(f.prins, domain.PrincipalFromContext(ctx))
	return nil
}

func inboundFixture(t *testing.T) (*InboundService, *fakeBinder, *fakeTurns) {
	t.Helper()
	b, turns := &fakeBinder{}, &fakeTurns{}
	s := NewInboundService(b, registered(), turns)
	n := 0
	s.newID = func() string { n++; return "conv-fixed" }
	return s, b, turns
}

const tgAgent = "telegram_ingress"

// First contact opens a conversation and binds the reply address in the same
// write, so the very first reply already knows where to go.
func TestAccept_FirstContactOpensAndBinds(t *testing.T) {
	s, b, turns := inboundFixture(t)

	if err := s.Accept(context.Background(), InboundMessage{Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:12345", Text: "book me a flight"}); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if b.createN != 1 {
		t.Fatalf("expected one conversation opened, got %d", b.createN)
	}
	got := b.convs[0]
	if got.Delivery.IngressAgentID != tgAgent || got.Delivery.ExternalID != "tg:12345" {
		t.Errorf("address not bound at open: %+v", got.Delivery)
	}
	if got.Profile != domain.ProfileCustomer {
		t.Errorf("an outsider entry point must not open an employee conversation: %q", got.Profile)
	}
	if len(turns.ran) != 1 || turns.ran[0].text != "book me a flight" {
		t.Errorf("the turn did not run: %+v", turns.ran)
	}
}

// The second message from the same sender continues the same conversation
// instead of starting a new one — otherwise every message loses its history.
func TestAccept_SecondMessageContinuesTheConversation(t *testing.T) {
	s, b, turns := inboundFixture(t)
	ctx := context.Background()

	_ = s.Accept(ctx, InboundMessage{Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:12345", Text: "book me a flight"})
	_ = s.Accept(ctx, InboundMessage{Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:12345", Text: "tomorrow please"})

	if b.createN != 1 {
		t.Fatalf("expected one conversation, got %d", b.createN)
	}
	if len(turns.ran) != 2 || turns.ran[0].conv != turns.ran[1].conv {
		t.Errorf("turns landed in different conversations: %+v", turns.ran)
	}
}

// Two different senders on the same ingress are two different conversations, and
// two different owners — otherwise one person's transcript is another's.
func TestAccept_SendersAreIsolated(t *testing.T) {
	b, turns := &fakeBinder{}, &fakeTurns{}
	s := NewInboundService(b, registered(), turns)
	n := 0
	s.newID = func() string { n++; return "conv-" + string(rune('a'+n)) }
	ctx := context.Background()

	_ = s.Accept(ctx, InboundMessage{Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:11111", Text: "hello"})
	_ = s.Accept(ctx, InboundMessage{Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:22222", Text: "hello"})

	if b.createN != 2 {
		t.Fatalf("expected two conversations, got %d", b.createN)
	}
	if b.convs[0].OwnerID == b.convs[1].OwnerID {
		t.Errorf("two senders must not share an owner: %q", b.convs[0].OwnerID)
	}
}

// The namespace bound, on the inbound side: a Telegram bridge cannot inject a
// message claiming to be a Slack user.
func TestAccept_RefusesOutsideTheNamespace(t *testing.T) {
	s, b, turns := inboundFixture(t)

	err := s.Accept(context.Background(), InboundMessage{Sender: domain.AgentPrincipal(tgAgent), ExternalID: "slack:U42", Text: "hello"})
	if !errors.Is(err, domain.ErrOutsideNamespace) {
		t.Fatalf("expected ErrOutsideNamespace, got %v", err)
	}
	// Nothing is created for a refused message, so a misconfiguration leaves no
	// orphan conversations to clean up.
	if b.createN != 0 || len(turns.ran) != 0 {
		t.Errorf("a refused message must leave no trace: %d convs, %d turns", b.createN, len(turns.ran))
	}
}

// An ordinary agent's signal is not ingress traffic. The caller uses this to fall
// through to normal signal handling, so this must be distinguishable.
func TestAccept_UnregisteredSenderFallsThrough(t *testing.T) {
	s, b, _ := inboundFixture(t)

	err := s.Accept(context.Background(), InboundMessage{Sender: domain.AgentPrincipal("scout_agent"), ExternalID: "tg:1", Text: "hello"})
	if !errors.Is(err, ErrNotAnIngress) {
		t.Fatalf("expected ErrNotAnIngress, got %v", err)
	}
	if b.createN != 0 {
		t.Error("an ordinary agent must not open a conversation through this path")
	}
}

func TestAccept_NoRegistryMeansNothingIsAnIngress(t *testing.T) {
	s := NewInboundService(&fakeBinder{}, nil, &fakeTurns{})
	if err := s.Accept(context.Background(), InboundMessage{Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:1", Text: "hi"}); !errors.Is(err, ErrNotAnIngress) {
		t.Errorf("expected ErrNotAnIngress, got %v", err)
	}
}

// A closed conversation is not silently reopened: an ended transcript stays
// ended, and the sender starts a new one.
func TestAccept_ClosedConversationStartsAFreshOne(t *testing.T) {
	b, turns := &fakeBinder{}, &fakeTurns{}
	b.convs = append(b.convs, domain.Conversation{
		ID: "conv-old", Status: domain.ConversationClosed,
		Delivery: domain.DeliveryAddress{IngressAgentID: tgAgent, ExternalID: "tg:12345"},
	})
	s := NewInboundService(b, registered(), turns)
	s.newID = func() string { return "conv-new" }

	if err := s.Accept(context.Background(), InboundMessage{Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:12345", Text: "hello again"}); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if len(turns.ran) != 1 || turns.ran[0].conv != "conv-new" {
		t.Errorf("expected a fresh conversation, got %+v", turns.ran)
	}
}

func TestAccept_RejectsEmptyText(t *testing.T) {
	s, _, _ := inboundFixture(t)
	if err := s.Accept(context.Background(), InboundMessage{Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:1", Text: "  "}); err == nil {
		t.Error("an empty inbound message must not start a turn")
	}
}

// A store failure must surface rather than silently dropping the message.
func TestAccept_StoreFailureSurfaces(t *testing.T) {
	b := &fakeBinder{findErr: errors.New("database down")}
	s := NewInboundService(b, registered(), &fakeTurns{})
	if err := s.Accept(context.Background(), InboundMessage{Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:1", Text: "hi"}); err == nil {
		t.Error("a store failure must not look like a delivered message")
	}
}

// A sender bound to a principal speaks AS that principal: the turn context
// carries the bound identity — the same hop the MCP middleware performs —
// instead of the chat worker's own agent identity.
func TestAccept_BoundSenderRunsUnderTheBoundPrincipal(t *testing.T) {
	s, _, turns := inboundFixture(t)
	s.SetIdentityResolver(boundIdentities{bound: true, binding: domain.IdentityBinding{
		Surface: "chat:telegram", ExternalID: "tg:12345",
		BoundToKind: domain.BindPrincipal, BoundToID: "afsin",
	}})

	if err := s.Accept(context.Background(), InboundMessage{Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:12345", Text: "check my laptop"}); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if len(turns.prins) != 1 || turns.prins[0] != domain.AgentPrincipal("afsin") {
		t.Errorf("the turn must run under the bound principal; got %+v", turns.prins)
	}
}

// A group binding names a container, not a speaker: the sender keeps its own
// identity and the group's terms reach it through the decision point.
func TestAccept_GroupBindingDoesNotImpersonateTheGroup(t *testing.T) {
	s, _, turns := inboundFixture(t)
	s.SetIdentityResolver(boundIdentities{bound: true, binding: domain.IdentityBinding{
		Surface: "chat:telegram", ExternalID: "tg:12345",
		BoundToKind: domain.BindGroup, BoundToID: "support-team",
	}})

	if err := s.Accept(context.Background(), InboundMessage{Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:12345", Text: "hello"}); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if len(turns.prins) != 1 || turns.prins[0] != (domain.PrincipalRef{}) {
		t.Errorf("a group binding must not stamp a principal; got %+v", turns.prins)
	}
}

// An unbound sender under the surface-default stranger policy is admitted
// exactly as before: no principal is stamped, the surface stays the identity.
func TestAccept_UnboundSenderKeepsSurfaceIdentity(t *testing.T) {
	s, _, turns := inboundFixture(t)
	s.SetIdentityResolver(boundIdentities{bound: false, mode: domain.StrangerSurfaceDefault})

	if err := s.Accept(context.Background(), InboundMessage{Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:12345", Text: "hello"}); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if len(turns.prins) != 1 || turns.prins[0] != (domain.PrincipalRef{}) {
		t.Errorf("an unbound sender must not run under a principal; got %+v", turns.prins)
	}
}

// refuse_until_bound still refuses an unbound sender before any conversation
// exists — the binding hop must not soften the stranger policy.
func TestAccept_UnboundSenderStillRefusedWhenPolicySaysSo(t *testing.T) {
	s, b, turns := inboundFixture(t)
	s.SetIdentityResolver(boundIdentities{bound: false, mode: domain.StrangerRefuseUntilBound})

	err := s.Accept(context.Background(), InboundMessage{Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:12345", Text: "hello"})
	if !errors.Is(err, ErrSenderNotBound) {
		t.Fatalf("expected ErrSenderNotBound, got %v", err)
	}
	if b.createN != 0 || len(turns.ran) != 0 {
		t.Errorf("a refused sender must leave no trace: %d convs, %d turns", b.createN, len(turns.ran))
	}
}

// A blocked binding is still dropped at the door, bound principal or not.
func TestAccept_BlockedBindingStillRefuses(t *testing.T) {
	s, b, turns := inboundFixture(t)
	s.SetIdentityResolver(boundIdentities{bound: true, binding: domain.IdentityBinding{
		Surface: "chat:telegram", ExternalID: "tg:12345",
		BoundToKind: domain.BindPrincipal, BoundToID: "afsin", Blocked: true,
	}})

	err := s.Accept(context.Background(), InboundMessage{Sender: domain.AgentPrincipal(tgAgent), ExternalID: "tg:12345", Text: "hello"})
	if !domain.IsBlockedSender(err) {
		t.Fatalf("expected a blocked-sender refusal, got %v", err)
	}
	if b.createN != 0 || len(turns.ran) != 0 {
		t.Errorf("a blocked sender must leave no trace: %d convs, %d turns", b.createN, len(turns.ran))
	}
}
