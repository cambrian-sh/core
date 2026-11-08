package domain

import "context"

// Private by default (ADR-0142 D12; closes
// T-20261106-agent-writes-during-a-conversation-owner-s-turn).
//
// An agent working a person's conversation reads that person's owner-fenced
// rows (it reads AS the conversation owner) and then writes notes, summaries,
// facts and artifacts under its OWN principal. Before D12 those writes carried
// only the agent's DefaultWriteTags, so material derived from a person's
// private ingest became readable by every principal the ceiling admitted.
//
// Now: when the kernel resolved the conversation the work is for (from the
// presented lease, never from a payload — INV-5) and the deployment runs private
// by default, the request context is MARKED with that owner, and every write
// chokepoint (EnforcingStoreWriter, EnforcingKnowledgeWriter, the document
// store, artifact classification) appends owner:<owner> to the decision point's
// classification. The owner fence then makes the row readable only by that
// person. The stamp is added AFTER the decision point classifies — it is not a
// hint the decision point could reject as coinage, and no writer can remove it.
//
// The conversation is the honest unit (D12 rejected per-sentence provenance).
// Ingress pseudonym owners (unbound senders) keep their pseudonym, which still
// isolates. Sharing to team/org knowledge is the explicit, policy-checked
// publish (ShareToTeam), never a side effect of a write.

// allowAllOwners is who an OSS (unscoped) reader acts as for the owner fence:
// itself when it is a person, plus the conversation owner it works for.
func allowAllOwners(ctx context.Context, principal PrincipalRef) []string {
	var owners []string
	if principal.Kind == PrincipalUser && principal.ID != "" {
		owners = append(owners, principal.ID)
	}
	if o := ConversationOwnerFromContext(ctx); o != "" && (len(owners) == 0 || owners[0] != o) {
		owners = append(owners, o)
	}
	return owners
}

type privateWriteCtxKey struct{}

// WithPrivateWrites marks ctx: every write classified under it is owner-scoped
// to owner. "" leaves ctx unchanged.
func WithPrivateWrites(ctx context.Context, owner string) context.Context {
	if owner == "" {
		return ctx
	}
	return context.WithValue(ctx, privateWriteCtxKey{}, owner)
}

// PrivateWriteOwnerFromContext returns the owner writes under ctx are private
// to, "" when unmarked.
func PrivateWriteOwnerFromContext(ctx context.Context) string {
	v, _ := ctx.Value(privateWriteCtxKey{}).(string)
	return v
}

// StampPrivateOwner returns final with owner:<owner> appended when ctx is
// marked (and the tag is not already there). It never removes a tag and never
// mutates final's backing array.
func StampPrivateOwner(ctx context.Context, final []string) []string {
	owner := PrivateWriteOwnerFromContext(ctx)
	if owner == "" {
		return final
	}
	tag := OwnerTag(owner)
	for _, t := range final {
		if t == tag {
			return final
		}
	}
	out := make([]string, 0, len(final)+1)
	out = append(out, final...)
	return append(out, tag)
}
