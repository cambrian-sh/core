package ingress

import (
	"context"

	"github.com/cambrian-sh/core/domain"
)

// ConversationSurfaces answers "which entry point did this conversation arrive
// on?" by following its bound delivery address to the ingress registration.
//
// This closes a gap that was invisible because everything downstream of it kept
// working. A chat turn has no task session — its lease carries a ConversationID
// and an empty SessionID — so session-based surface resolution found nothing and
// the request kept the transport-derived surface, `agent`. Every turn from a
// Telegram user was therefore authorised as an ordinary agent call: the policy
// linked to `surface:chat:telegram` was never consulted, and no decision was ever
// attributed to the entry point the message came through.
//
// The registration is re-read rather than trusted from the stored address, for
// the same reason DeliveryFor re-checks the namespace at delivery time: an
// ingress that has since been deregistered must stop conferring its surface, and
// a stored string cannot know that.
type ConversationSurfaces struct {
	convs     domain.ConversationStore
	ingresses domain.IngressResolver
}

// NewConversationSurfaces builds the reader. Either dependency may be nil, in
// which case resolution always reports "unknown" — which correctly falls back to
// the transport surface rather than inventing one.
func NewConversationSurfaces(convs domain.ConversationStore, ingresses domain.IngressResolver) *ConversationSurfaces {
	return &ConversationSurfaces{convs: convs, ingresses: ingresses}
}

// ConversationSurface returns the surface a conversation arrived on.
//
// The second return is false for a conversation that never came through an
// ingress — a console conversation has no delivery address — and that is not a
// failure: it means the transport-derived surface is the right answer.
func (c *ConversationSurfaces) ConversationSurface(ctx context.Context, conversationID string) (domain.SurfaceRef, bool) {
	if c == nil || c.convs == nil || c.ingresses == nil || conversationID == "" {
		return domain.SurfaceRef{}, false
	}
	conv, err := c.convs.GetConversation(ctx, conversationID)
	if err != nil || conv == nil || conv.Delivery.IsZero() {
		return domain.SurfaceRef{}, false
	}
	reg, ok := c.ingresses.ResolveIngress(ctx, domain.AgentPrincipal(conv.Delivery.IngressAgentID))
	if !ok || reg.IsZero() {
		// The ingress is gone. Reporting "unknown" rather than the stored surface
		// is the safe direction: a deregistered entry point must stop conferring
		// the reach its surface had.
		return domain.SurfaceRef{}, false
	}
	if reg.Surface.Kind == "" && reg.Surface.ID == "" {
		return domain.SurfaceRef{}, false
	}
	return reg.Surface, true
}
