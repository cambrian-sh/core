package app

import (
	"context"

	"github.com/cambrian-sh/core/domain"
)

// conversationProfileOf resolves a lease binding to the PROFILE of the conversation it
// was issued under — on the lease, or on its task session (so a plan delegated from a
// conversation carries it too) — exactly as conversationOwnerOf resolves the owner
// (network.Server.ConversationProfile; ADR-0084 D7, ENG-614). "" when the work is not
// for a conversation or the conversation cannot be read.
func conversationProfileOf(conversations domain.ConversationStore, sessions notesSessionReader) func(context.Context, domain.LeaseBinding) domain.ConversationProfile {
	return func(ctx context.Context, b domain.LeaseBinding) domain.ConversationProfile {
		if conversations == nil {
			return ""
		}
		convID := b.ConversationID
		if convID == "" && b.SessionID != "" && sessions != nil {
			if ses, err := sessions.GetSession(ctx, b.SessionID); err == nil && ses != nil {
				convID = ses.ConversationID
			}
		}
		if convID == "" {
			return ""
		}
		if c, err := conversations.GetConversation(ctx, convID); err == nil && c != nil {
			return c.Profile
		}
		return ""
	}
}
