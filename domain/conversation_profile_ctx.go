package domain

import "context"

type conversationProfileCtxKey struct{}

// WithConversationProfile stamps the profile of the conversation the work on ctx is
// for. It is set SERVER-SIDE only — from the caller's lease binding on the agent plane
// (the same lookup that stamps the conversation owner), never from a request field —
// so a worker cannot claim a wider profile. The decision point narrows reads by it
// (authz.ProfileNarrowingAuthorizer; ADR-0084 D7, owner decision 2026-11-14, ENG-614).
// "" leaves ctx unchanged.
func WithConversationProfile(ctx context.Context, p ConversationProfile) context.Context {
	if p == "" {
		return ctx
	}
	return context.WithValue(ctx, conversationProfileCtxKey{}, p)
}

// ConversationProfileFromContext returns the stamped conversation profile, or "".
func ConversationProfileFromContext(ctx context.Context) ConversationProfile {
	if ctx == nil {
		return ""
	}
	p, _ := ctx.Value(conversationProfileCtxKey{}).(ConversationProfile)
	return p
}
