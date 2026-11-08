package domain

import (
	"strings"
)

// The knowledge commit bridge (T-20261107-knowledge-substrate-extensions-for-case;
// investigation 2026-11-07 §7 gap 2).
//
// A proposal is stored FIRST as a candidate item asserted by its proposer (an agent
// or app acting for a person); an approval of kind ApprovalKindKnowledgeCommit
// names that candidate in its Object (approval Args are not persisted, so the
// candidate is what survives a restart). When an eligible approver approves, the
// kernel handler writes the COMMITTED item: same kind, entity, values and validity,
// AssertedBy = the approver the hub authenticated (never a caller-supplied value),
// SourceRef = KnowledgeCommitSourceRefPrefix + approval id (deterministic, so a
// re-run handler is a replay, not a second commit). Deny and expiry write nothing.
//
// Nothing here knows what a "decision" is: the kind, its predicates, its lifecycle
// and who may commit it are all data (KindSpec, the approvals config).

// ApprovalKindKnowledgeCommit is the approval kind (and default action class) of a
// knowledge commit. KindSpec.CommitClass may route a kind under its own class.
const ApprovalKindKnowledgeCommit = "knowledge_commit"

// KnowledgeCommitSourceRefPrefix marks an item the knowledge_commit handler wrote.
// Kernel-reserved: the plugin seam refuses it (authz.DesignatedActorGuard) and the
// proposal path never produces it, so DesignatedActors.ApprovedViaCommit admits only
// genuinely approved commits.
const KnowledgeCommitSourceRefPrefix = "knowledge_commit:"

// KnowledgeProposalSourceRefPrefix marks a candidate written by the proposal path.
const KnowledgeProposalSourceRefPrefix = "knowledge_proposal:"

// IsKnowledgeCommitRef reports whether a SourceRef is a kernel commit marker.
func IsKnowledgeCommitRef(sourceRef string) bool {
	return strings.HasPrefix(sourceRef, KnowledgeCommitSourceRefPrefix)
}

// ApproverActor renders an approval's ResolvedBy (ApproverLabel: a person by bare
// id, any other principal as kind:id) as a knowledge actor id in PrincipalRef.String
// form ("user:alice"). "" for an empty label and for a standing-approval marker
// ("standing:<id>"), which names a grant, not the person who answered.
func ApproverActor(resolvedBy string) string {
	switch {
	case resolvedBy == "", strings.HasPrefix(resolvedBy, "standing:"):
		return ""
	case strings.Contains(resolvedBy, ":"):
		return resolvedBy
	}
	return UserPrincipal(resolvedBy).String()
}
