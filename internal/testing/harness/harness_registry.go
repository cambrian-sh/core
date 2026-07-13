package harness

import (
	"context"

	"github.com/cambrian-sh/core/domain"
)

// HarnessRegistry is an in-memory agent registry for test harness use.
type HarnessRegistry struct {
	agents    map[string]domain.AgentDefinition
	manifests map[string]domain.AgentManifest
}

func NewHarnessRegistry() *HarnessRegistry {
	return &HarnessRegistry{
		agents:    make(map[string]domain.AgentDefinition),
		manifests: make(map[string]domain.AgentManifest),
	}
}

func (r *HarnessRegistry) Register(agent domain.AgentDefinition, manifest domain.AgentManifest) {
	r.agents[agent.ID] = agent
	r.manifests[agent.ID] = manifest
}

func (r *HarnessRegistry) GetAllAgents(_ context.Context) ([]domain.AgentDefinition, error) {
	out := make([]domain.AgentDefinition, 0, len(r.agents))
	for _, a := range r.agents {
		out = append(out, a)
	}
	return out, nil
}

func (r *HarnessRegistry) GetManifest(_ context.Context, agentID string) (*domain.AgentManifest, error) {
	if m, ok := r.manifests[agentID]; ok {
		return &m, nil
	}
	return nil, nil
}

func (r *HarnessRegistry) GetAgentByName(_ context.Context, name string) (*domain.AgentDefinition, error) {
	if a, ok := r.agents[name]; ok {
		return &a, nil
	}
	return nil, nil
}

func (r *HarnessRegistry) SetProvisional(string, bool) error { return nil }

var _ domain.AgentRegistry = (*HarnessRegistry)(nil)
