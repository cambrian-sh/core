package metabolism

import (
	"context"
	"fmt"
	"sort"

	"github.com/cambrian-sh/core/domain"
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

// GetAllAgents returns every registered agent, ordered by ID.
//
// Sorted, because the production registry is sorted and a double that is not
// makes tests depend on Go's randomised map iteration. Selection has no explicit
// tiebreak: `FindCandidates` scans this slice and the argmax keeps the first of
// an equal-scoring set, so two agents that tie on merit are decided purely by
// the order they arrive in. Under bbolt that order is `ForEach` over sorted
// keys, so production picks the same agent every time; under a map it changed
// per call, and the same step could bind a different agent on a retry.
//
// That is how TestServer_StepFn_FallbackUsesRunnerUpWhenWinnerFails failed
// roughly one run in four: the self-healer re-ran the step, selection re-ran
// with it, and the second attempt sometimes landed on the other agent — so the
// retry the test was asserting never happened. The test was right; the double
// was lying about the order production sees.
func (r *InMemoryRegistry) GetAllAgents(_ context.Context) ([]domain.AgentDefinition, error) {
	list := make([]domain.AgentDefinition, 0, len(r.agents))
	for _, agent := range r.agents {
		list = append(list, agent)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
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
