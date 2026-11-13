package authz

import (
	"context"
	"errors"
	"fmt"

	"github.com/cambrian-sh/core/domain"
)

// ErrForgedAssertion is returned by DesignatedActorGuard for an item that would
// count as a designated (committed) assertion but did not come through a kernel path.
var ErrForgedAssertion = errors.New("authz: only the kernel may write a designated assertion")

// DesignatedActorGuard is the plugin seam's fence for kinds resolved by the
// designated_actor authority (T-20261107-knowledge-substrate-extensions-for-case).
//
// The plugin seam (KernelServices.Knowledge) stores a caller's AssertedBy and
// SourceRef verbatim (T-20261103-kernelservices-knowledge-hands-plugins-the-raw). For
// a designated kind that would let any in-process plugin write "the approver said so":
// an item asserted by a designated principal, or one carrying the kernel's commit
// marker. This guard refuses exactly those two shapes, so the kernel paths — the
// knowledge_commit handler, which stamps the approver the hub authenticated — are the
// only way to produce a designated principal's word or an approved commit. Candidate
// items by any other actor (a detector, a proposal) still pass: they are what a
// designated kind exists to keep below the committed value.
//
// Sources (DesignatedActors.Sources) still pass: a system of record asserting as itself
// is trusted in-process code, the link mechanism ceiling's trust model. Reads pass
// through unchanged — read fencing is not this guard's job.
type DesignatedActorGuard struct {
	inner domain.KnowledgeStore
	kinds domain.KindSource
}

var _ domain.KnowledgeStore = (*DesignatedActorGuard)(nil)

// NewDesignatedActorGuard wraps inner. kinds is the live ontology or a fixed
// registry; nil declares no kinds, so the guard passes everything through.
func NewDesignatedActorGuard(inner domain.KnowledgeStore, kinds domain.KindSource) *DesignatedActorGuard {
	if kinds == nil {
		kinds = (*domain.KindRegistry)(nil)
	}
	return &DesignatedActorGuard{inner: inner, kinds: kinds}
}

// CheckPluginWrite reports why item may not be written through the plugin seam, or nil.
// Exported so other non-kernel write paths can apply the same rule.
func CheckPluginWrite(reg *domain.KindRegistry, item domain.KnowledgeItem) error {
	if domain.IsKnowledgeCommitRef(item.SourceRef) {
		return fmt.Errorf("knowledge put: kind %q: source_ref prefix %q is reserved for the knowledge_commit handler: %w",
			item.Kind, domain.KnowledgeCommitSourceRefPrefix, ErrForgedAssertion)
	}
	spec, ok := reg.Spec(item.Kind)
	if !ok || spec.Policy != domain.ResolutionPolicyDesignatedActor || spec.Designated == nil {
		return nil
	}
	for _, p := range spec.Designated.Principals {
		if item.AssertedBy == p {
			return fmt.Errorf("knowledge put: kind %q: %q is a designated principal; its word is written only through knowledge_commit: %w",
				item.Kind, p, ErrForgedAssertion)
		}
	}
	return nil
}

// PutItem — refused for a forged designated assertion.
func (g *DesignatedActorGuard) PutItem(ctx context.Context, item domain.KnowledgeItem) (domain.KnowledgeItemID, bool, error) {
	if err := CheckPluginWrite(g.kinds.KindsFor(item.NamespaceID), item); err != nil {
		return "", false, err
	}
	return g.inner.PutItem(ctx, item)
}

// GetItem — PASS-THROUGH.
func (g *DesignatedActorGuard) GetItem(ctx context.Context, id domain.KnowledgeItemID) (*domain.KnowledgeItem, error) {
	return g.inner.GetItem(ctx, id)
}

// CurrentResolutions — PASS-THROUGH.
func (g *DesignatedActorGuard) CurrentResolutions(ctx context.Context, namespace, kind, policy string) ([]domain.Resolution, error) {
	return g.inner.CurrentResolutions(ctx, namespace, kind, policy)
}

// EraseItems — PASS-THROUGH (erasure is a compliance act, not an assertion).
func (g *DesignatedActorGuard) EraseItems(ctx context.Context, namespace, kind string, sel domain.KnowledgeErasure) (int, error) {
	return g.inner.EraseItems(ctx, namespace, kind, sel)
}
