package interview

import (
	"context"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// testRegistry implements ManifestReader for interview tests.
// Tests can set manifests[agentID] directly to control GetManifest output.
type testRegistry struct {
	manifests map[string]*domain.AgentManifest
}

func newTestRegistry(_ ...domain.AgentDefinition) *testRegistry {
	return &testRegistry{manifests: make(map[string]*domain.AgentManifest)}
}

func (r *testRegistry) GetManifest(_ context.Context, agentID string) (*domain.AgentManifest, error) {
	man, ok := r.manifests[agentID]
	if !ok {
		return &domain.AgentManifest{}, nil
	}
	return man, nil
}

// newMockManifestReader creates a testRegistry with pre-configured manifests.
func newMockManifestReader(manifests map[string]*domain.AgentManifest) *testRegistry {
	if manifests == nil {
		manifests = make(map[string]*domain.AgentManifest)
	}
	return &testRegistry{manifests: manifests}
}

// mockProfileStore implements ProfileStore.
type mockProfileStore struct {
	savedProfiles   []savedProfileCall
	profileToReturn *domain.AgentProfile
	judicialRecords []string
	embeddingDistFn func(a, b []float32) float64
}

type savedProfileCall struct {
	AgentID    string
	SourceHash string
	Embedding  []float32
	Profile    domain.AgentProfile
}

func (s *mockProfileStore) SaveProfile(_ context.Context, agentID, sourceHash string, embedding []float32, profile domain.AgentProfile) error {
	s.savedProfiles = append(s.savedProfiles, savedProfileCall{
		AgentID:    agentID,
		SourceHash: sourceHash,
		Embedding:  embedding,
		Profile:    profile,
	})
	return nil
}

func (s *mockProfileStore) GetProfile(_ context.Context, _, _ string) (*domain.AgentProfile, error) {
	return s.profileToReturn, nil
}

func (s *mockProfileStore) GetJudicialRecords(_ context.Context, _, _ string, _ int) ([]string, error) {
	return s.judicialRecords, nil
}

func (s *mockProfileStore) Save(_ context.Context, _ string, _ []float32, _ map[string]interface{}) error {
	return nil
}

func (s *mockProfileStore) EmbeddingDistance(a, b []float32) float64 {
	if s.embeddingDistFn != nil {
		return s.embeddingDistFn(a, b)
	}
	return 0.0
}

// mockUpdater records SetProvisional calls.
type mockUpdater struct {
	calls []setProvisionalCall
}

type setProvisionalCall struct {
	AgentID     string
	Provisional bool
}

func (u *mockUpdater) SetProvisional(agentID string, provisional bool) error {
	u.calls = append(u.calls, setProvisionalCall{agentID, provisional})
	return nil
}

// mockEmbedder returns fixed vectors per call index, cycling on the last element.
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

// trackingEmbedder wraps mockEmbedder and records whether Embed was called.
type trackingEmbedder struct {
	called bool
	inner  *mockEmbedder
}

func (e *trackingEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	e.called = true
	return e.inner.Embed(ctx, text)
}

// mockCardFetcher implements CardFetcher.
type mockCardFetcher struct {
	card *domain.AgentCard
	err  error
}

func (m *mockCardFetcher) FetchCard(_ context.Context, _ string) (*domain.AgentCard, error) {
	return m.card, m.err
}

// mockInterviewRequester implements domain.ProposalRequester.
type mockInterviewRequester struct {
	capturedRequests []domain.ProposalRequest
}

func (m *mockInterviewRequester) RequestProposalFrom(_ context.Context, _ domain.AgentDefinition, req domain.ProposalRequest) (domain.ProposalResponse, error) {
	m.capturedRequests = append(m.capturedRequests, req)
	return domain.ProposalResponse{
		Confidence:         0.9,
		Rationale:          "interview mock",
		EstimatedLatencyMs: 50,
	}, nil
}

// fakeInterviewSearcher implements domain.InterviewSearcher for tests.
type fakeInterviewSearcher struct {
	results map[string]float64 // agentID → similarity
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

// newWorker wires mock dependencies into an InterviewWorker.
func newWorker(registry *testRegistry, embedder *mockEmbedder, store *mockProfileStore, updater *mockUpdater) *InterviewWorker {
	return NewInterviewWorker(registry, embedder, store, updater)
}

func abs64(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// mockSweepTrigger records TriggerSweep calls.
type mockSweepTrigger struct {
	callCount int
}

func (m *mockSweepTrigger) TriggerSweep() {
	m.callCount++
}
