package clusterer

import (
	"context"
	"testing"
)

// ── Test doubles ────────────────────────────────────────────────────────────

type mockSource struct {
	agents []AgentEmbedding
}

func (s *mockSource) GetAllAgentEmbeddings(_ context.Context) ([]AgentEmbedding, error) {
	return s.agents, nil
}

type mockStore struct {
	capabilities map[string][]string // agentID → caps
	clusterNames map[string]string   // repID → name
	setCapsCount int
	setNameCount int
}

func newMockStore() *mockStore {
	return &mockStore{
		capabilities: make(map[string][]string),
		clusterNames: make(map[string]string),
	}
}

func (s *mockStore) SetCapabilities(agentID string, caps []string) error {
	s.capabilities[agentID] = caps
	s.setCapsCount++
	return nil
}

func (s *mockStore) SetClusterName(repID string, name string) error {
	s.clusterNames[repID] = name
	s.setNameCount++
	return nil
}

func (s *mockStore) GetClusterName(repID string) (string, error) {
	return s.clusterNames[repID], nil
}

type mockGenerator struct {
	callCount int
	response  string
}

func (g *mockGenerator) Generate(_ context.Context, _ string) (string, error) {
	g.callCount++
	if g.response == "" {
		return "generated-cluster", nil
	}
	return g.response, nil
}


// ── Cycle 2: MinAgents guard — skip all writes when registry is too small ───

// TestRunSweep_BelowMinAgents_NoWrites verifies that when eligible agent count
// is below minAgents, runSweep makes zero SetCapabilities or SetClusterName calls.
func TestRunSweep_BelowMinAgents_NoWrites(t *testing.T) {
	source := &mockSource{agents: []AgentEmbedding{
		{AgentID: "agent-x", SourceHash: "sha-x", Embedding: []float32{1.0, 0.0}},
		{AgentID: "agent-y", SourceHash: "sha-y", Embedding: []float32{0.99, 0.14}},
	}}
	store := newMockStore()
	gen := &mockGenerator{}

	c := New(source, store, gen, 0.80, 0.02, 3) // minAgents=3, only 2 present
	if err := c.runSweep(context.Background()); err != nil {
		t.Fatalf("runSweep returned error: %v", err)
	}

	if store.setCapsCount != 0 {
		t.Errorf("expected 0 SetCapabilities calls below minAgents, got %d", store.setCapsCount)
	}
	if store.setNameCount != 0 {
		t.Errorf("expected 0 SetClusterName calls below minAgents, got %d", store.setNameCount)
	}
	if gen.callCount != 0 {
		t.Errorf("expected 0 Generator calls below minAgents, got %d", gen.callCount)
	}
}

// ── Cycle 3: Sticky Representative retains crown when delta < epsilon ────────

// TestDetermineRepresentative_StickyRetainsCrown verifies that when the previous
// representative's average-similarity score is within epsilon of the best scorer,
// the previous representative is kept (vocabulary stability).
func TestDetermineRepresentative_StickyRetainsCrown(t *testing.T) {
	// agent-a and agent-b are almost identical; both score ~1.0 avg similarity.
	// With epsilon=0.02, agent-a (previous rep) should be retained.
	members := []AgentEmbedding{
		{AgentID: "agent-a", Embedding: []float32{1.0, 0.0}},
		{AgentID: "agent-b", Embedding: []float32{0.9999, 0.0141}},
		{AgentID: "agent-c", Embedding: []float32{0.998, 0.063}},
	}
	got := determineRepresentative(members, "agent-a", 0.02)
	if got != "agent-a" {
		t.Errorf("expected previous rep agent-a to retain crown (delta < epsilon), got %q", got)
	}
}

// ── Cycle 4: Challenger takes crown when delta >= epsilon ─────────────────────

// TestDetermineRepresentative_ChallengerWins verifies that when the challenger's
// average similarity exceeds the previous rep's score by at least epsilon, the
// challenger becomes the new representative.
func TestDetermineRepresentative_ChallengerWins(t *testing.T) {
	// agent-centroid is placed at the true centre; agent-outlier is far off-axis.
	// agent-centroid should comfortably beat agent-outlier by >> epsilon=0.02.
	members := []AgentEmbedding{
		{AgentID: "agent-centroid", Embedding: []float32{1.0, 0.0}},
		{AgentID: "agent-near",     Embedding: []float32{0.99, 0.14}},
		{AgentID: "agent-outlier",  Embedding: []float32{0.0, 1.0}},
	}
	// Previous rep is agent-outlier which is cosine-far from the others.
	got := determineRepresentative(members, "agent-outlier", 0.02)
	if got == "agent-outlier" {
		t.Errorf("expected challenger to win crown from outlier prev-rep, got %q", got)
	}
}

// ── Cycle 5: Previous rep absent from cluster → handover forced ───────────────

// TestDetermineRepresentative_PrevRepAbsent verifies that when the previous
// representative is no longer in the cluster (e.g., agent was deleted), the
// best-scoring current member is elected unconditionally.
func TestDetermineRepresentative_PrevRepAbsent(t *testing.T) {
	members := []AgentEmbedding{
		{AgentID: "agent-x", Embedding: []float32{1.0, 0.0}},
		{AgentID: "agent-y", Embedding: []float32{0.99, 0.14}},
	}
	got := determineRepresentative(members, "agent-gone", 0.02)
	if got == "agent-gone" {
		t.Errorf("expected new rep when prev-rep is absent, still got %q", got)
	}
	if got != "agent-x" && got != "agent-y" {
		t.Errorf("expected one of the current members as rep, got %q", got)
	}
}

// ── Cycle 6: Fingerprint cache hit — second identical sweep skips Generator ───

// TestRunSweep_FingerprintUnchanged_NoGeneratorCall verifies that when the
// agent registry is identical between two sweeps AND the cluster name is already
// cached in the store, the Generator is not called on the second sweep.
func TestRunSweep_FingerprintUnchanged_NoGeneratorCall(t *testing.T) {
	agents := []AgentEmbedding{
		{AgentID: "agent-a", SourceHash: "sha-a", Embedding: []float32{1.0, 0.0}, Description: "summariser"},
		{AgentID: "agent-b", SourceHash: "sha-b", Embedding: []float32{0.99, 0.14}, Description: "condenser"},
		{AgentID: "agent-c", SourceHash: "sha-c", Embedding: []float32{1.0, 0.05}, Description: "writer"},
	}
	source := &mockSource{agents: agents}
	store := newMockStore()
	gen := &mockGenerator{response: "text-processing"}

	c := New(source, store, gen, 0.80, 0.02, 3)

	// First sweep — Generator must be called, cluster name cached.
	if err := c.runSweep(context.Background()); err != nil {
		t.Fatalf("first runSweep error: %v", err)
	}
	firstCallCount := gen.callCount
	if firstCallCount == 0 {
		t.Fatal("expected Generator call on first sweep, got 0")
	}

	// Second sweep — same agents, same hashes → fingerprint unchanged → cache hit.
	if err := c.runSweep(context.Background()); err != nil {
		t.Fatalf("second runSweep error: %v", err)
	}
	if gen.callCount != firstCallCount {
		t.Errorf("expected no additional Generator calls on second sweep, got %d new calls",
			gen.callCount-firstCallCount)
	}
}

// ── Cycle 7: TraitModel agents excluded from clustering ───────────────────────

// TestRunSweep_TraitModel_Excluded verifies that agents with Trait="model" are
// not included in any cluster and do not receive SetCapabilities calls.
func TestRunSweep_TraitModel_Excluded(t *testing.T) {
	source := &mockSource{agents: []AgentEmbedding{
		{AgentID: "model-agent", SourceHash: "sha-m", Embedding: []float32{1.0, 0.0}, Trait: "model"},
		{AgentID: "agent-a",    SourceHash: "sha-a", Embedding: []float32{1.0, 0.0}},
		{AgentID: "agent-b",    SourceHash: "sha-b", Embedding: []float32{0.99, 0.14}},
		{AgentID: "agent-c",    SourceHash: "sha-c", Embedding: []float32{1.0, 0.05}},
	}}
	store := newMockStore()
	gen := &mockGenerator{response: "text-cluster"}

	c := New(source, store, gen, 0.80, 0.02, 3)
	if err := c.runSweep(context.Background()); err != nil {
		t.Fatalf("runSweep error: %v", err)
	}

	if _, ok := store.capabilities["model-agent"]; ok {
		t.Error("model-agent should not receive SetCapabilities, but it did")
	}
}

// ── Cycle 8: Singleton cluster → nil capabilities ────────────────────────────

// TestRunSweep_SingletonCluster_NilCapabilities verifies that an agent with no
// similar peer receives SetCapabilities(agentID, nil) — empty capability slice.
func TestRunSweep_SingletonCluster_NilCapabilities(t *testing.T) {
	// agent-lone has cosine-distance > threshold from all others; forms singleton.
	source := &mockSource{agents: []AgentEmbedding{
		{AgentID: "agent-lone", SourceHash: "sha-l", Embedding: []float32{0.0, 1.0}},
		{AgentID: "agent-a",   SourceHash: "sha-a", Embedding: []float32{1.0, 0.0}},
		{AgentID: "agent-b",   SourceHash: "sha-b", Embedding: []float32{0.99, 0.14}},
		{AgentID: "agent-c",   SourceHash: "sha-c", Embedding: []float32{1.0, 0.05}},
	}}
	store := newMockStore()
	gen := &mockGenerator{response: "text-cluster"}

	c := New(source, store, gen, 0.80, 0.02, 4) // minAgents=4, all 4 are eligible
	if err := c.runSweep(context.Background()); err != nil {
		t.Fatalf("runSweep error: %v", err)
	}

	caps, called := store.capabilities["agent-lone"]
	if !called {
		t.Fatal("expected SetCapabilities called for singleton agent-lone, but it was not")
	}
	if len(caps) != 0 {
		t.Errorf("expected nil/empty capabilities for singleton agent, got %v", caps)
	}
}

// ── Cycle 1 (Tracer Bullet): two similar agents cluster together ─────────────

// TestRunSweep_TwoSimilarAgents_SetCapabilitiesCalled is the tracer bullet:
// two agents with cosine similarity above threshold both receive a capability
// via SetCapabilities after a sweep.
func TestRunSweep_TwoSimilarAgents_SetCapabilitiesCalled(t *testing.T) {
	// {1,0} and {0.99, 0.14} have cosine similarity ≈ 0.99 — well above 0.80
	source := &mockSource{agents: []AgentEmbedding{
		{AgentID: "agent-a", SourceHash: "sha-a", Embedding: []float32{1.0, 0.0}, Description: "text summariser"},
		{AgentID: "agent-b", SourceHash: "sha-b", Embedding: []float32{0.99, 0.14}, Description: "document condenser"},
		{AgentID: "agent-c", SourceHash: "sha-c", Embedding: []float32{1.0, 0.05}, Description: "abstract writer"},
	}}
	store := newMockStore()
	gen := &mockGenerator{response: "text-processing"}

	c := New(source, store, gen, 0.80, 0.02, 3)
	if err := c.runSweep(context.Background()); err != nil {
		t.Fatalf("runSweep returned error: %v", err)
	}

	if store.setCapsCount == 0 {
		t.Error("expected SetCapabilities to be called at least once, got 0")
	}
	// All three agents should receive a capability
	for _, id := range []string{"agent-a", "agent-b", "agent-c"} {
		if len(store.capabilities[id]) == 0 {
			t.Errorf("agent %q: expected non-empty capabilities, got nil/empty", id)
		}
	}
}
