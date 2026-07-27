package authz

import (
	"context"

	"github.com/cambrian-sh/core/domain"
)

// ClassifyArtifactWrite derives an artifact's authoritative classification from
// the decision point, given the writer's requested tags. It is the artifact-path
// twin of EnforcingStoreWriter (used by the UploadArtifact RPC and by tool
// artifact promotion): the writer may only narrow, never broaden.
func ClassifyArtifactWrite(ctx context.Context, a domain.Authorizer, principal domain.PrincipalRef, hint []string) ([]string, error) {
	if a == nil {
		a = domain.AllowAllAuthorizer{}
	}
	final, dec := a.ClassifyWrite(ctx, principal, hint)
	if !dec.Allowed {
		return nil, ErrWriteDenied
	}
	return final, nil
}

// FilterArtifacts returns only the artifacts a reader holding this predicate may
// see, applying the same three-set/CNF test as the vector read path over the
// artifact's Tags. A nil predicate is fail-closed (returns nothing); a bypass
// predicate returns everything.
func FilterArtifacts(eff *domain.TagPredicate, artifacts []domain.Artifact) []domain.Artifact {
	if eff == nil {
		return nil // fail-closed
	}
	out := make([]domain.Artifact, 0, len(artifacts))
	for _, a := range artifacts {
		if eff.Allows(a.Tags) {
			out = append(out, a)
		}
	}
	return out
}

// ArtifactReadable reports whether a single artifact is visible under this
// predicate. Convenience for GetArtifact (single-hash lookup).
func ArtifactReadable(eff *domain.TagPredicate, a domain.Artifact) bool {
	return eff.Allows(a.Tags)
}

// ArtifactContextRefs projects visible artifacts into ContextRef discovery
// entries for working_memory (REQ-SDK-007b, Criterion #21). This is a best-effort
// DISCOVERY layer — the authoritative read gate remains GetArtifact, so an
// artifact surfaced here that the consuming agent cannot actually read is still
// denied at fetch time. Out-of-predicate artifacts are omitted up front.
func ArtifactContextRefs(eff *domain.TagPredicate, arts []domain.Artifact, stepLabel string) []domain.ContextRef {
	visible := FilterArtifacts(eff, arts)
	out := make([]domain.ContextRef, 0, len(visible))
	for _, a := range visible {
		labels := make([]string, 0, len(a.Tags)+1)
		labels = append(labels, stepLabel)
		labels = append(labels, a.Tags...)
		out = append(out, domain.ContextRef{
			CID:       domain.CID(a.Hash),
			Type:      "agent_artifact",
			Labels:    labels,
			Precision: 1.0,
			Snippet:   a.SemanticSummary,
		})
	}
	return out
}
