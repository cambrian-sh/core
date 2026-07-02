package gatekeeper

import (
	"context"

	"github.com/cambrian-sh/cambrian-runtime/internal/config"
	"github.com/cambrian-sh/cambrian-runtime/domain"
)

func defaultGatekeeperCfg() config.ExecutionConfig {
	return config.ExecutionConfig{
		GatekeeperW1:            0.4,
		GatekeeperW2:            0.4,
		GatekeeperW3:            0.2,
		GatekeeperMaxCandidates: 0,
		ContextGrowthK:          0.001,
	}
}

func defaultTestExecCfg() config.ExecutionConfig {
	return config.ExecutionConfig{
		GatekeeperW1:            0.4,
		GatekeeperW2:            0.4,
		GatekeeperW3:            0.2,
		GatekeeperMaxCandidates: 5,
		ContextGrowthK:          0.001,
	}
}

type mockEmbedder struct {
	results [][]float32
	callIdx int
}

func (e *mockEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	if len(e.results) == 0 {
		return []float32{0.1, 0.2, 0.3}, nil
	}
	idx := e.callIdx
	if idx >= len(e.results) {
		idx = len(e.results) - 1
	}
	e.callIdx++
	return e.results[idx], nil
}

type mockGatekeeperProfileReader struct {
	profiles map[string]*domain.AgentProfile
}

func (m *mockGatekeeperProfileReader) GetProfile(_ context.Context, agentID, sourceHash string) (*domain.AgentProfile, error) {
	if m.profiles == nil {
		return nil, nil
	}
	return m.profiles[agentID+":"+sourceHash], nil
}

type fakeInterviewSearcher struct {
	results map[string]float64
}

func (f *fakeInterviewSearcher) SearchByEmbedding(_ context.Context, _ []float32, threshold float64, _ int) ([]domain.AgentSearchResult, error) {
	var out []domain.AgentSearchResult
	for agentID, sim := range f.results {
		if sim < threshold {
			continue
		}
		out = append(out, domain.AgentSearchResult{AgentID: agentID, Similarity: sim})
	}
	return out, nil
}

type mockAgentDeclarationSource struct {
	agents    []domain.AgentDefinition
	Agents    map[string]domain.AgentDefinition
	manifests map[string]*domain.AgentManifest
}

func newAgentSource() *mockAgentDeclarationSource {
	return &mockAgentDeclarationSource{
		Agents:    make(map[string]domain.AgentDefinition),
		manifests: make(map[string]*domain.AgentManifest),
	}
}

func newAgentSourceWith(agents ...domain.AgentDefinition) *mockAgentDeclarationSource {
	m := newAgentSource()
	for _, a := range agents {
		m.SetAgent(a)
	}
	return m
}

func newMockAgentDeclarationSource(agents []domain.AgentDefinition, manifests map[string]*domain.AgentManifest) *mockAgentDeclarationSource {
	if manifests == nil {
		manifests = make(map[string]*domain.AgentManifest)
	}
	agentsMap := make(map[string]domain.AgentDefinition)
	for _, a := range agents {
		agentsMap[a.ID] = a
	}
	return &mockAgentDeclarationSource{agents: agents, Agents: agentsMap, manifests: manifests}
}

func (m *mockAgentDeclarationSource) SetAgent(a domain.AgentDefinition) {
	if m.Agents == nil {
		m.Agents = make(map[string]domain.AgentDefinition)
	}
	m.Agents[a.ID] = a
	m.agents = append(m.agents, a)
}

func (m *mockAgentDeclarationSource) GetAllAgents(_ context.Context) ([]domain.AgentDefinition, error) {
	if len(m.agents) > 0 {
		return m.agents, nil
	}
	var list []domain.AgentDefinition
	for _, a := range m.Agents {
		list = append(list, a)
	}
	return list, nil
}

func (m *mockAgentDeclarationSource) GetManifest(_ context.Context, agentID string) (*domain.AgentManifest, error) {
	man, ok := m.manifests[agentID]
	if !ok {
		return &domain.AgentManifest{}, nil
	}
	return man, nil
}

func candidateIDs(cs []domain.ScoredCandidate) []string {
	ids := make([]string, len(cs))
	for i, c := range cs {
		ids[i] = c.Agent.ID
	}
	return ids
}

func abs64(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
