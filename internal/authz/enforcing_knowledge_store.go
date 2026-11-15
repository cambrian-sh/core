package authz

import (
	"context"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
)

// EnforcingKnowledgeStore is the WRITE chokepoint of the knowledge substrate's PLUGIN
// seam (KernelServices.Knowledge; T-20261103-kernelservices-knowledge-hands-plugins-the-raw,
// ENG-449). Before it, the seam handed plugins the raw store, which stored an item's
// classification exactly as the producer supplied it.
//
// PutItem — ENFORCED: the item's classification is a hint and the decision point's
// ClassifyWrite answer replaces it, for the principal on ctx — the same
// classification as EnforcingKnowledgeWriter (classifyKnowledgeItem). Producers run
// as a deterministic SOURCE service principal (owner decision 2026-11-15, ENG-494):
// the evidence outbox consumer stamps the evidence's source, and the kernel's
// per-plugin seam falls back to source:<plugin>:<namespace> when ctx carries none
// (app.knowledgeSourceFallback), so the evidence classification is the hint of a
// real writer, never of no one.
//
// GetItem, CurrentResolutions, EraseItems — PASS-THROUGH, labelled: read fencing of
// this seam is the scoped query plane's job (ADR-0118), and erasure is a compliance
// path that removes, never classifies. The kernel-reserved kinds and designated
// assertions stay fenced by ReservedKindGuard and DesignatedActorGuard, composed
// around this store.
type EnforcingKnowledgeStore struct {
	inner  domain.KnowledgeStore
	authz  domain.Authorizer
	logger *slog.Logger
}

var _ domain.KnowledgeStore = (*EnforcingKnowledgeStore)(nil)

// NewEnforcingKnowledgeStore wraps inner. A nil authorizer is the OSS allow-all
// default; a nil logger is slog.Default().
func NewEnforcingKnowledgeStore(inner domain.KnowledgeStore, a domain.Authorizer, logger *slog.Logger) *EnforcingKnowledgeStore {
	if a == nil {
		a = domain.AllowAllAuthorizer{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &EnforcingKnowledgeStore{inner: inner, authz: a, logger: logger}
}

// PutItem — ENFORCED (classified write).
func (s *EnforcingKnowledgeStore) PutItem(ctx context.Context, item domain.KnowledgeItem) (domain.KnowledgeItemID, bool, error) {
	item, err := classifyKnowledgeItem(ctx, s.authz, s.logger, item)
	if err != nil {
		return "", false, err
	}
	return s.inner.PutItem(ctx, item)
}

// GetItem — PASS-THROUGH (see the type comment).
func (s *EnforcingKnowledgeStore) GetItem(ctx context.Context, id domain.KnowledgeItemID) (*domain.KnowledgeItem, error) {
	return s.inner.GetItem(ctx, id)
}

// CurrentResolutions — PASS-THROUGH (see the type comment).
func (s *EnforcingKnowledgeStore) CurrentResolutions(ctx context.Context, namespace, kind, policy string) ([]domain.Resolution, error) {
	return s.inner.CurrentResolutions(ctx, namespace, kind, policy)
}

// EraseItems — PASS-THROUGH (see the type comment).
func (s *EnforcingKnowledgeStore) EraseItems(ctx context.Context, namespace, kind string, sel domain.KnowledgeErasure) (int, error) {
	return s.inner.EraseItems(ctx, namespace, kind, sel)
}
