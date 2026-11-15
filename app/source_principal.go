package app

import (
	"context"
	"log/slog"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// reactiveIngestConnector names the source of content the reactive signal receiver
// ingests through the unscoped KernelServices.Ingestor (ENG-494).
const reactiveIngestConnector = "reactive"

// scopePluginServices bounds the per-plugin copy of the services bundle to the
// plugin's own source identity (ENG-494 / ENG-449, owner decision 2026-11-15): its
// memory ingests run as source:<plugin>:<doc source>, and its knowledge writes made
// with no principal on ctx run as source:<plugin>:<namespace>. Enforced here, in the
// seam, for the same reason scopedAgentRegistrar is: a guarantee a plugin is merely
// trusted to keep is only a comment.
func scopePluginServices(pluginID string, svc KernelServices) KernelServices {
	if k, ok := svc.Ingestor.(*kernelMemoryIngestor); ok {
		svc.Ingestor = k.forConnector(pluginID)
	}
	if svc.Knowledge != nil {
		svc.Knowledge = ScopeKnowledgeToSource(pluginID, svc.Knowledge)
	}
	return svc
}

// NewPluginKnowledgeSeam composes KernelServices.Knowledge over the raw substrate
// store: the memory_fact kind fenced off (ReservedKindGuard), forged designated
// assertions refused (DesignatedActorGuard), and every item write classified by the
// decision point (authz.EnforcingKnowledgeStore, ENG-449). The kernel builds the
// seam only through this function; it is exported so an end-to-end test of a
// policy plugin composes exactly what the kernel hands plugins.
func NewPluginKnowledgeSeam(raw domain.KnowledgeStore, kinds domain.KindSource, a domain.Authorizer) domain.KnowledgeStore {
	return authz.NewReservedKindGuard(authz.NewDesignatedActorGuard(
		authz.NewEnforcingKnowledgeStore(raw, a, slog.Default()), kinds), domain.KindMemoryFact)
}

// ScopeKnowledgeToSource is the per-plugin view of the seam: a knowledge write that
// carries no principal runs as source:<connector>:<namespace> (knowledgeSourceFallback).
func ScopeKnowledgeToSource(connector string, seam domain.KnowledgeStore) domain.KnowledgeStore {
	return knowledgeSourceFallback{inner: seam, connector: connector}
}

// knowledgeSourceFallback stamps a source service principal on a knowledge write
// that carries no principal, so the seam's ClassifyWrite (authz.EnforcingKnowledgeStore)
// classifies a real writer — a premium decision point refuses a write by no one
// (no_principal). A principal already on ctx (the evidence outbox consumer's
// evidence source, a person's or agent's request) is kept: it is the more precise
// answer to "who wrote this".
type knowledgeSourceFallback struct {
	inner     domain.KnowledgeStore
	connector string
}

var _ domain.KnowledgeStore = knowledgeSourceFallback{}

func (k knowledgeSourceFallback) PutItem(ctx context.Context, item domain.KnowledgeItem) (domain.KnowledgeItemID, bool, error) {
	if domain.PrincipalFromContext(ctx).IsZero() {
		ctx = domain.WithPrincipal(ctx, domain.SourcePrincipal(k.connector, item.NamespaceID))
	}
	return k.inner.PutItem(ctx, item)
}

func (k knowledgeSourceFallback) GetItem(ctx context.Context, id domain.KnowledgeItemID) (*domain.KnowledgeItem, error) {
	return k.inner.GetItem(ctx, id)
}

func (k knowledgeSourceFallback) CurrentResolutions(ctx context.Context, namespace, kind, policy string) ([]domain.Resolution, error) {
	return k.inner.CurrentResolutions(ctx, namespace, kind, policy)
}

func (k knowledgeSourceFallback) EraseItems(ctx context.Context, namespace, kind string, sel domain.KnowledgeErasure) (int, error) {
	return k.inner.EraseItems(ctx, namespace, kind, sel)
}
