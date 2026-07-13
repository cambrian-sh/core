package agentmgr

import (
	"context"
	"fmt"

	"github.com/cambrian-sh/core/domain"
)

// testRegistry is a simple in-memory implementation of domain.AgentRegistry
// used only for unit tests in this package.
type testRegistry struct {
	agents    map[string]domain.AgentDefinition
	manifests map[string]*domain.AgentManifest
}

func newTestRegistry() *testRegistry {
	return &testRegistry{
		agents:    make(map[string]domain.AgentDefinition),
		manifests: make(map[string]*domain.AgentManifest),
	}
}

func (r *testRegistry) GetAgentByName(_ context.Context, name string) (*domain.AgentDefinition, error) {
	agent, ok := r.agents[name]
	if !ok {
		return nil, fmt.Errorf("agent not found: %s", name)
	}
	return &agent, nil
}

func (r *testRegistry) GetAllAgents(_ context.Context) ([]domain.AgentDefinition, error) {
	var list []domain.AgentDefinition
	for _, agent := range r.agents {
		list = append(list, agent)
	}
	return list, nil
}

func (r *testRegistry) GetManifest(_ context.Context, agentID string) (*domain.AgentManifest, error) {
	m, ok := r.manifests[agentID]
	if !ok {
		return &domain.AgentManifest{}, nil
	}
	return m, nil
}

// newTestManager returns a minimal AgentManager for tests.
func newTestManager() *AgentManager {
	return &AgentManager{
		Registry:        newTestRegistry(),
		InstanceManager: NewInstanceManager("python", ""),
		AgentConnector:  NewAgentConnector(),
		daemons:         newDaemonRegistry(),
		stopped:         newStoppedSet(),
	}
}
