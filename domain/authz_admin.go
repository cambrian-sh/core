package domain

import "context"

// PolicyAdmin is the ADMINISTRATION surface (PAP) a policy plugin contributes:
// authoring scopes, listing the controlled vocabulary, and answering "why can
// this principal see this?".
//
// It follows the same shape as domain.WatchConfigHandler (CORE-OPS-1): the proto
// and the RPC shells live in OSS so the operator contract is stable, and the
// handler is nil in an OSS build — those RPCs then return Unimplemented rather
// than silently pretending to have applied a policy. A UI discovers which are
// live through the capability handshake, not by trial and error.
type PolicyAdmin interface {
	// SetAgentScope sets an agent's intrinsic access boundary. It must reject a
	// statically unsatisfiable scope at SAVE time — an administrator who writes a
	// policy that can never match anything has to be told now, not discover it
	// through an empty result three days later (ADR-0085 D14).
	SetAgentScope(ctx context.Context, agentID string, required, anyOf, forbidden []string) error

	// SetAgentWriteTags sets the classification ceiling stamped on an agent's
	// writes. The agent may narrow within it and can never broaden.
	SetAgentWriteTags(ctx context.Context, agentID string, tags []string) error

	// Vocabulary lists the controlled classification tags. It backs the selection
	// UI; a free-text tag field is a defect (ADR-0085 D11).
	Vocabulary(ctx context.Context) []string

	// ValidateTag reports whether a tag may be applied. Used by the operator's
	// tag-memory command so a typo is refused at the boundary.
	ValidateTag(tag string) bool

	// ExplainAccess answers a hypothetical access question without performing it —
	// the gpresult / "Check access" analogue (ADR-0085 D8).
	ExplainAccess(ctx context.Context, req AccessRequest) AccessDecision
}
