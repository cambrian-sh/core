package scope

import "github.com/cambrian-sh/cambrian-runtime/domain"

// AuthorizeArtifactWrite derives an artifact's kernel-authoritative classification
// from the writer's DefaultWriteTags (optionally narrowed by the agent hint), and
// returns the tag set to persist with kernel-stamped provenance. It is the
// artifact-path twin of ScopedStoreWriter (used by the UploadArtifact RPC).
// The agent cannot broaden — it may only narrow. ADR-0035 (C2).
func AuthorizeArtifactWrite(ws WriterScope, vocab *Vocabulary, narrowHint []string) ([]string, error) {
	final, err := DeriveWriteTags(ws.DefaultWriteTags, narrowHint, vocab)
	if err != nil {
		return nil, err
	}
	return StampSourceProvenance(final, ws.WriterID), nil
}

// FilterArtifactsByScope returns only the artifacts a reader with the given
// effective scope may see, applying the same three-set/CNF predicate as the
// vector read path (EffectiveScope.Allows over the artifact's Tags). A nil scope
// is fail-closed (returns nothing); ScopeSystem returns everything. ADR-0034 (D12).
func FilterArtifactsByScope(eff *domain.EffectiveScope, artifacts []domain.Artifact) []domain.Artifact {
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

// ArtifactReadable reports whether a single artifact is visible to a reader with
// the given effective scope. Convenience for GetArtifact (single-hash lookup).
func ArtifactReadable(eff *domain.EffectiveScope, a domain.Artifact) bool {
	return eff != nil && eff.Allows(a.Tags)
}

// ArtifactContextRefs projects scope-visible artifacts into ContextRef discovery
// entries for working_memory (ADR-0034 / REQ-SDK-007b, Criterion #21). This is a
// best-effort DISCOVERY layer — the authoritative read gate remains GetArtifact
// (agent_scope), so an artifact surfaced here that the consuming agent cannot
// actually read will still be denied at fetch time. Out-of-scope artifacts are
// omitted up front via FilterArtifactsByScope.
func ArtifactContextRefs(eff *domain.EffectiveScope, arts []domain.Artifact, stepLabel string) []domain.ContextRef {
	visible := FilterArtifactsByScope(eff, arts)
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
