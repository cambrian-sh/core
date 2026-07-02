package domain

import "context"

// AgentLister returns all registered agent definitions.
type AgentLister interface {
	GetAllAgents(ctx context.Context) ([]AgentDefinition, error)
}

// ManifestReader returns an agent's A2A Manifest.
// It is the minimal interface needed by components that check Declaration
// compatibility (e.g. Gatekeeper).
type ManifestReader interface {
	GetManifest(ctx context.Context, agentID string) (*AgentManifest, error)
}

// AgentResolver looks up a single agent by name.
type AgentResolver interface {
	GetAgentByName(ctx context.Context, name string) (*AgentDefinition, error)
}

// AgentUpdater persists provisional-state transitions for an agent.
type AgentUpdater interface {
	SetProvisional(agentID string, provisional bool) error
}

// AgentPruner deletes an agent from the registry. Used by the startup reconcile
// to evict orphans — a model dropped from config, or an agent whose source file
// was deleted — so they stop competing in the auction. Idempotent: deleting an
// absent id is not an error.
type AgentPruner interface {
	DeleteAgent(agentID string) error
}

// AgentDeclarationSource is the subset of AgentRegistry needed by Gatekeeper:
// listing + manifest lookup without single-agent resolution.
type AgentDeclarationSource interface {
	AgentLister
	ManifestReader
}

// AgentRegistry is the full agent catalogue interface.
type AgentRegistry interface {
	AgentLister
	ManifestReader
	AgentResolver
}
