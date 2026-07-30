package ingress_test

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
	"github.com/cambrian-sh/core/internal/ingress"
)

type convStore struct{ conv *domain.Conversation }

func (c convStore) GetConversation(_ context.Context, id string) (*domain.Conversation, error) {
	if c.conv == nil || c.conv.ID != id {
		return nil, domain.ErrConversationNotFound
	}
	return c.conv, nil
}

// Only GetConversation is exercised; the rest satisfies the interface.
func (convStore) CreateConversation(context.Context, domain.Conversation) error { return nil }
func (convStore) ListConversations(context.Context, string, int) ([]domain.Conversation, error) {
	return nil, nil
}
func (convStore) SetConversationStatus(context.Context, string, domain.ConversationStatus) error {
	return nil
}
func (convStore) AppendMessage(context.Context, domain.Message) (domain.Message, error) {
	return domain.Message{}, nil
}
func (convStore) ListMessages(context.Context, string, int64, int) ([]domain.Message, error) {
	return nil, nil
}
func (convStore) FindByDelivery(context.Context, domain.DeliveryAddress) (*domain.Conversation, error) {
	return nil, domain.ErrConversationNotFound
}
func (convStore) BindDelivery(context.Context, string, domain.DeliveryAddress) error { return nil }
func (convStore) GetMessageByClientID(context.Context, string, string) (domain.Message, error) {
	return domain.Message{}, domain.ErrConversationNotFound
}

type ingressRegistry map[string]domain.IngressRegistration

func (r ingressRegistry) ResolveIngress(_ context.Context, p domain.PrincipalRef) (domain.IngressRegistration, bool) {
	reg, ok := r[p.ID]
	return reg, ok
}

const tgSurfaceKind, tgSurfaceID = "chat", "telegram"

func telegramConv() *domain.Conversation {
	return &domain.Conversation{
		ID: "conv-1",
		Delivery: domain.DeliveryAddress{
			IngressAgentID: "telegram_ingress_agent",
			ExternalID:     "tg:6484759603",
		},
	}
}

func registered() ingressRegistry {
	return ingressRegistry{
		"telegram_ingress_agent": {
			AgentID:   "telegram_ingress_agent",
			Surface:   domain.SurfaceRef{Kind: tgSurfaceKind, ID: tgSurfaceID},
			Namespace: []string{"tg:"},
		},
	}
}

// The gap this closes: a chat turn has no task session, so surface resolution
// found nothing and every turn from a Telegram user was authorised as an
// ordinary agent call — the policy on `surface:chat:telegram` was never
// consulted and no decision was attributed to the entry point.
func TestConversationSurface_ResolvesTheEntryPoint(t *testing.T) {
	cs := ingress.NewConversationSurfaces(convStore{conv: telegramConv()}, registered())

	got, ok := cs.ConversationSurface(context.Background(), "conv-1")
	if !ok {
		t.Fatal("a conversation bound to a registered ingress must resolve its surface")
	}
	if got.Kind != tgSurfaceKind || got.ID != tgSurfaceID {
		t.Fatalf("want chat:telegram, got %s", got.String())
	}
}

// A console conversation has no delivery address. That is not a failure — the
// transport-derived surface is the right answer for it.
func TestConversationSurface_ConsoleConversationHasNone(t *testing.T) {
	cs := ingress.NewConversationSurfaces(
		convStore{conv: &domain.Conversation{ID: "conv-1"}}, registered())

	if _, ok := cs.ConversationSurface(context.Background(), "conv-1"); ok {
		t.Fatal("a conversation that never came through an ingress must not claim a surface")
	}
}

// A deregistered entry point must stop conferring the reach its surface had. The
// stored address cannot know it is gone, so the registration is re-read.
func TestConversationSurface_DeregisteredIngressConfersNothing(t *testing.T) {
	cs := ingress.NewConversationSurfaces(convStore{conv: telegramConv()}, ingressRegistry{})

	if _, ok := cs.ConversationSurface(context.Background(), "conv-1"); ok {
		t.Fatal("a deregistered ingress still conferred its surface")
	}
}

// Narrowest-first: a task session's own surface wins over the conversation's, and
// both win over the transport. Widening on the way in is the escalation the clamp
// exists to prevent.
func TestResolveSurfaceForTurn_PrefersTheConversationOverTheTransport(t *testing.T) {
	cs := ingress.NewConversationSurfaces(convStore{conv: telegramConv()}, registered())

	ctx := domain.WithSurface(context.Background(),
		domain.SurfaceRef{Kind: domain.SurfaceAgent, ID: "grpc"})

	got := authz.ResolveSurfaceForTurn(ctx, nil, cs, "conv-1")
	if got.Kind != tgSurfaceKind || got.ID != tgSurfaceID {
		t.Fatalf("the transport surface won over the conversation's: %s", got.String())
	}
}

func TestResolveSurfaceForTurn_FallsBackToTheTransport(t *testing.T) {
	cs := ingress.NewConversationSurfaces(convStore{conv: telegramConv()}, registered())

	ctx := domain.WithSurface(context.Background(),
		domain.SurfaceRef{Kind: domain.SurfaceAgent, ID: "grpc"})

	// No conversation in play: an ordinary agent call must keep its own surface
	// rather than inheriting somebody's chat.
	got := authz.ResolveSurfaceForTurn(ctx, nil, cs, "")
	if got.Kind != domain.SurfaceAgent {
		t.Fatalf("want the transport surface, got %s", got.String())
	}
}
