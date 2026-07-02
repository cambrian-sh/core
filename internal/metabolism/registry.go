package metabolism

import (
	"context"
	"fmt"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// InMemoryRegistry is a test/seed registry backed by in-process maps.
// Production code uses the bbolt-backed registry in internal/storage.
type InMemoryRegistry struct {
	agents    map[string]domain.AgentDefinition
	manifests map[string]*domain.AgentManifest
}

func NewInMemoryRegistry() *InMemoryRegistry {
	return &InMemoryRegistry{
		agents:    make(map[string]domain.AgentDefinition),
		manifests: make(map[string]*domain.AgentManifest),
	}
}

func (r *InMemoryRegistry) GetAgentByName(_ context.Context, name string) (*domain.AgentDefinition, error) {
	agent, ok := r.agents[name]
	if !ok {
		return nil, fmt.Errorf("agent not found in registry: %s", name)
	}
	return &agent, nil
}

func (r *InMemoryRegistry) GetAllAgents(_ context.Context) ([]domain.AgentDefinition, error) {
	var list []domain.AgentDefinition
	for _, agent := range r.agents {
		list = append(list, agent)
	}
	return list, nil
}

func (r *InMemoryRegistry) GetManifest(_ context.Context, agentID string) (*domain.AgentManifest, error) {
	m, ok := r.manifests[agentID]
	if !ok {
		return &domain.AgentManifest{}, nil
	}
	return m, nil
}

func (r *InMemoryRegistry) SetManifest(agentID string, m *domain.AgentManifest) {
	r.manifests[agentID] = m
}

func (r *InMemoryRegistry) SetAgent(agent domain.AgentDefinition) {
	r.agents[agent.ID] = agent
}

func (r *InMemoryRegistry) SetProvisional(agentID string, provisional bool) error {
	return nil
}
