package app

import (
	"context"

	"github.com/cambrian-sh/core/domain"
	corechat "github.com/cambrian-sh/core/internal/chat"
	"github.com/cambrian-sh/core/internal/substrate/operator"
)

// conversationOps binds the operator chat lane (ADR-0084 D9) to the kernel's conversation
// store and turn service. It is the composition-root adapter that keeps the operator package
// free of kernel handles — the mirror of SessionOpsFuncs for the conversation surface.
//
// SendTurn routes through the TurnService (the stateless worker POOL), NOT the task planner:
// this is the whole point of the OSS chat lane, and the fix for the operator-plane planner
// bug where a chat message was decomposed into a plan.
type conversationOps struct {
	store domain.ConversationStore
	turns *corechat.TurnService
}

var _ operator.ConversationOps = (*conversationOps)(nil)

// Open creates the conversation if absent (owner = principal, supplied by the caller from
// the resolved operator principal). An existing conversation owned by a different principal
// is a permission error; an existing one owned by the same principal is a no-op that reports
// existed=true, so a client retry of Open is idempotent on the conversation id.
func (c *conversationOps) Open(ctx context.Context, id, ownerID, title, profile, policy string) (bool, error) {
	existing, err := c.store.GetConversation(ctx, id)
	if err == nil {
		if existing.OwnerID != ownerID {
			return false, operator.ErrConversationForbidden
		}
		return true, nil
	}
	if err != domain.ErrConversationNotFound {
		return false, err
	}
	return false, c.store.CreateConversation(ctx, domain.Conversation{
		ID:      id,
		OwnerID: ownerID,
		Title:   title,
		Status:  domain.ConversationOpen,
		Profile: domain.ConversationProfile(profile),
		Policy:  policy,
	})
}

// SendTurn runs one turn on the pool and returns the persisted agent reply. clientID is the
// idempotency key (a retry returns the original reply).
func (c *conversationOps) SendTurn(ctx context.Context, id, text, clientID string) (domain.Message, error) {
	return c.turns.Turn(ctx, corechat.TurnRequest{
		ConversationID: id,
		Text:           text,
		ClientID:       clientID,
	})
}

func (c *conversationOps) Close(ctx context.Context, id string) error {
	return c.store.SetConversationStatus(ctx, id, domain.ConversationClosed)
}

func (c *conversationOps) Messages(ctx context.Context, id string, afterSeq int64, limit int) ([]domain.Message, error) {
	return c.store.ListMessages(ctx, id, afterSeq, limit)
}

func (c *conversationOps) Owner(ctx context.Context, id string) (string, error) {
	conv, err := c.store.GetConversation(ctx, id)
	if err != nil {
		return "", err
	}
	return conv.OwnerID, nil
}

func (c *conversationOps) List(ctx context.Context, ownerID string, limit int) ([]domain.Conversation, error) {
	return c.store.ListConversations(ctx, ownerID, limit)
}
