package verify

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ── mockAgentSource implements VerifierRegistry ───────────────────────────────

type mockAgentSource struct {
	agents    map[string]domain.AgentDefinition
	manifests map[string]*domain.AgentManifest
}

func newAgentSource() *mockAgentSource {
	return &mockAgentSource{
		agents:    make(map[string]domain.AgentDefinition),
		manifests: make(map[string]*domain.AgentManifest),
	}
}

func newAgentSourceWith(agents ...domain.AgentDefinition) *mockAgentSource {
	m := newAgentSource()
	for _, a := range agents {
		m.SetAgent(a)
	}
	return m
}

func (m *mockAgentSource) SetAgent(a domain.AgentDefinition) {
	m.agents[a.ID] = a
}

func (m *mockAgentSource) GetAllAgents(_ context.Context) ([]domain.AgentDefinition, error) {
	var list []domain.AgentDefinition
	for _, a := range m.agents {
		list = append(list, a)
	}
	return list, nil
}

func (m *mockAgentSource) GetManifest(_ context.Context, agentID string) (*domain.AgentManifest, error) {
	man, ok := m.manifests[agentID]
	if !ok {
		return &domain.AgentManifest{}, nil
	}
	return man, nil
}

func (m *mockAgentSource) GetAgentByName(_ context.Context, name string) (*domain.AgentDefinition, error) {
	a, ok := m.agents[name]
	if !ok {
		return nil, nil
	}
	return &a, nil
}

// ── mockEmbedder returns a fixed vector for every call ────────────────────────

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

// ── vwMockGatekeeperReader implements ProfileReader ───────────────────────────

type vwMockGatekeeperReader struct {
	mu       sync.Mutex
	profiles map[string]*domain.AgentProfile
}

func (m *vwMockGatekeeperReader) GetProfile(_ context.Context, agentID, sourceHash string) (*domain.AgentProfile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.profiles == nil {
		return nil, nil
	}
	return m.profiles[agentID+":"+sourceHash], nil
}

// ── vwMockVerifyRequester ─────────────────────────────────────────────────────

type vwMockVerifyRequester struct {
	called      chan struct{}
	lastReq     *domain.VerifyRequest
	mu          sync.Mutex
	callCount   int
	returnScore float32
	returnCrit  string
}

func (m *vwMockVerifyRequester) VerifyOutput(_ context.Context, _ domain.AgentDefinition, req domain.VerifyRequest) (domain.VerifyResponse, error) {
	m.mu.Lock()
	m.lastReq = &req
	m.callCount++
	m.mu.Unlock()
	select {
	case m.called <- struct{}{}:
	default:
	}
	score := m.returnScore
	if score == 0 {
		score = 0.88
	}
	crit := m.returnCrit
	if crit == "" {
		crit = "looks good"
	}
	return domain.VerifyResponse{QualityScore: score, Critique: crit}, nil
}

// ── vwMockTaskEventRW ─────────────────────────────────────────────────────────

type vwMockTaskEventRW struct {
	mu     sync.Mutex
	events map[string]domain.TaskEvent
}

func (m *vwMockTaskEventRW) WriteTaskEvent(e domain.TaskEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events[e.TaskID] = e
	return nil
}

func (m *vwMockTaskEventRW) ReadTaskEvent(taskID string) (*domain.TaskEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.events[taskID]
	if !ok {
		return nil, nil
	}
	return &e, nil
}

// ── vwMockJudicialStore ───────────────────────────────────────────────────────

type vwMockJudicialStore struct {
	mu      sync.Mutex
	records []string
}

func (m *vwMockJudicialStore) Save(_ context.Context, text string, _ []float32, _ map[string]any) error {
	m.mu.Lock()
	m.records = append(m.records, text)
	m.mu.Unlock()
	return nil
}

// ── vwMockProfileStore ────────────────────────────────────────────────────────

type vwMockProfileStore struct {
	mu       sync.Mutex
	profiles map[string]*domain.AgentProfile
}

func (m *vwMockProfileStore) GetProfile(_ context.Context, agentID, sourceHash string) (*domain.AgentProfile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.profiles[agentID+":"+sourceHash], nil
}

func (m *vwMockProfileStore) SaveProfile(_ context.Context, agentID, sourceHash string, _ []float32, p domain.AgentProfile) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.profiles[agentID+":"+sourceHash] = &p
	return nil
}

// ── Shared helpers ────────────────────────────────────────────────────────────

func newTestVerifierPool(
	agents []domain.AgentDefinition,
	profiles map[string]*domain.AgentProfile,
	threshold float64,
) *VerifierPool {
	reg := newAgentSourceWith(agents...)
	pr := &vwMockGatekeeperReader{profiles: profiles}
	return NewVerifierPool(reg, pr, threshold, 3)
}

func newTestVerificationWorker(t *testing.T, queueCap int) *VerificationWorker {
	t.Helper()
	pool := &VerifierPool{
		Registry:      newAgentSource(),
		Profiles:      &vwMockGatekeeperReader{},
		Threshold:     0.8,
		RecencyWindow: 3,
	}
	return NewVerificationWorker(
		pool,
		&vwMockVerifyRequester{called: make(chan struct{}, 1)},
		&vwMockTaskEventRW{events: map[string]domain.TaskEvent{}},
		&vwMockJudicialStore{},
		&vwMockProfileStore{profiles: map[string]*domain.AgentProfile{}},
		&mockEmbedder{},
		VerificationWorkerConfig{
			QueueCapacity:         queueCap,
			VerifierRecencyWindow: 3,
			TrustBoostThreshold:   0.4,
		},
	)
}

func findNonFNVTaskID(t *testing.T) string {
	t.Helper()
	for i := 0; i < 10000; i++ {
		id := fmt.Sprintf("step-%d-planxyz", i)
		if !shouldSample(id, 1.0, 0.4) {
			return id
		}
	}
	t.Fatal("could not find non-FNV taskID in 10000 attempts")
	return ""
}

func findFNVTaskID(t *testing.T) string {
	t.Helper()
	for i := 0; i < 10000; i++ {
		id := fmt.Sprintf("step-%d-planfnv", i)
		if shouldSample(id, 1.0, 0.4) {
			return id
		}
	}
	t.Fatal("could not find FNV-sampled taskID in 10000 attempts")
	return ""
}

func findFNVTaskID2(t *testing.T, exclude string) string {
	t.Helper()
	for i := 0; i < 10000; i++ {
		id := fmt.Sprintf("step-%d-planfnv2", i)
		if id != exclude && shouldSample(id, 1.0, 0.4) {
			return id
		}
	}
	t.Fatal("could not find second FNV-sampled taskID in 10000 attempts")
	return ""
}
