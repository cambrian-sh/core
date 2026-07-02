package interview

import (
	"context"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// Test 5: After processAgent runs, Store.SaveProfile was called exactly once
// with the correct AgentID and SourceHash.
func TestProcessAgent_SaveProfileCalledOnce(t *testing.T) {
	agent := domain.AgentDefinition{ID: "agent-1", SourceHash: "hash-abc", Provisional: true}
	registry := newTestRegistry(agent)
	embedder := &mockEmbedder{results: [][]float32{{0.1, 0.2}}}
	store := &mockProfileStore{}
	updater := &mockUpdater{}

	w := newWorker(registry, embedder, store, updater)
	if err := w.processAgent(context.Background(), agent); err != nil {
		t.Fatalf("processAgent returned error: %v", err)
	}

	if len(store.savedProfiles) != 1 {
		t.Fatalf("expected SaveProfile called once, got %d times", len(store.savedProfiles))
	}
	saved := store.savedProfiles[0]
	if saved.AgentID != agent.ID {
		t.Errorf("expected agentID %q, got %q", agent.ID, saved.AgentID)
	}
	if saved.SourceHash != agent.SourceHash {
		t.Errorf("expected sourceHash %q, got %q", agent.SourceHash, saved.SourceHash)
	}
}

// Test 6: After processAgent runs, Updater.SetProvisional was called with
// (agentID, false).
func TestProcessAgent_SetProvisionalFalseCalled(t *testing.T) {
	agent := domain.AgentDefinition{ID: "agent-2", SourceHash: "hash-xyz", Provisional: true}
	registry := newTestRegistry(agent)
	embedder := &mockEmbedder{results: [][]float32{{0.1, 0.2}}}
	store := &mockProfileStore{}
	updater := &mockUpdater{}

	w := newWorker(registry, embedder, store, updater)
	if err := w.processAgent(context.Background(), agent); err != nil {
		t.Fatalf("processAgent returned error: %v", err)
	}

	if len(updater.calls) != 1 {
		t.Fatalf("expected 1 SetProvisional call, got %d", len(updater.calls))
	}
	call := updater.calls[0]
	if call.AgentID != agent.ID {
		t.Errorf("expected agentID %q in SetProvisional, got %q", agent.ID, call.AgentID)
	}
	if call.Provisional != false {
		t.Errorf("expected SetProvisional(false), got SetProvisional(%v)", call.Provisional)
	}
}

// Cycle 0033-03: TraitDaemon takes the fast-path (same as TraitTool) —
// embeds description, saves profile, marks Provisional=false, no LLM scenarios.
func TestProcessAgent_TraitDaemon_TakesFastPath(t *testing.T) {
	agent := domain.AgentDefinition{
		ID:          "gold-tracker",
		Description: "Polls gold price API and emits price signals",
		SourceHash:  "hash-daemon",
		Trait:       domain.TraitDaemon,
	}
	registry := newTestRegistry(agent)
	embedder := &mockEmbedder{results: [][]float32{{0.5, 0.6, 0.7}}}
	store := &mockProfileStore{}
	updater := &mockUpdater{}

	w := newWorker(registry, embedder, store, updater)
	if err := w.processAgent(context.Background(), agent); err != nil {
		t.Fatalf("processAgent: %v", err)
	}

	// Fast-path: exactly 1 SaveProfile call, TrustScore=1.0 (static, no Merit decay).
	if len(store.savedProfiles) != 1 {
		t.Fatalf("want 1 SaveProfile call (fast-path), got %d", len(store.savedProfiles))
	}
	savedProfile := store.savedProfiles[0].Profile
	if savedProfile.TrustScore != 1.0 {
		t.Errorf("TraitDaemon fast-path: want TrustScore=1.0, got %v (non-fast-path gives 0)", savedProfile.TrustScore)
	}
	if savedProfile.SuccessRate != 1.0 {
		t.Errorf("TraitDaemon fast-path: want SuccessRate=1.0, got %v", savedProfile.SuccessRate)
	}
	// SetProvisional(false) must be called.
	if len(updater.calls) != 1 || updater.calls[0].Provisional != false {
		t.Errorf("SetProvisional(false) must be called for daemon fast-path")
	}
}

// Test 7: Re-interview path: when a prior profile exists with SuccessRate=0.8
// and the decay fraction is 0.5, the new profile seeds SuccessRate=0.4.
func TestProcessAgent_ReinterviewDecay(t *testing.T) {
	agent := domain.AgentDefinition{
		ID:          "agent-3",
		SourceHash:  "hash-new",
		Provisional: true,
	}
	registry := newTestRegistry(agent)
	registry.manifests["agent-3"] = &domain.AgentManifest{
		ReleaseNotes: "Improved SQL performance",
	}

	embedder := &mockEmbedder{
		results: [][]float32{
			{0.1, 0.2, 0.3}, // scenarios embedding (call 0)
			{0.5, 0.5, 0.5}, // new release notes embedding (call 1)
			{0.0, 0.0, 0.0}, // prior source hash proxy (call 2)
		},
	}

	store := &mockProfileStore{
		profileToReturn: &domain.AgentProfile{
			AgentID:     "agent-3",
			SourceHash:  "hash-old",
			SuccessRate: 0.8,
			TrustScore:  0.6,
		},
		embeddingDistFn: func(_, _ []float32) float64 { return 0.5 },
	}
	updater := &mockUpdater{}

	w := newWorker(registry, embedder, store, updater)
	if err := w.processAgent(context.Background(), agent); err != nil {
		t.Fatalf("processAgent returned error: %v", err)
	}

	if len(store.savedProfiles) != 1 {
		t.Fatalf("expected 1 saved profile, got %d", len(store.savedProfiles))
	}
	got := store.savedProfiles[0].Profile

	const wantSuccessRate = 0.4 // 0.8 * 0.5
	if abs64(got.SuccessRate-wantSuccessRate) > 1e-9 {
		t.Errorf("expected seeded SuccessRate %.4f, got %.4f", wantSuccessRate, got.SuccessRate)
	}
	const wantTrustScore = 0.3 // 0.6 * 0.5
	if abs64(got.TrustScore-wantTrustScore) > 1e-9 {
		t.Errorf("expected seeded TrustScore %.4f, got %.4f", wantTrustScore, got.TrustScore)
	}
}

// Test 8: DecayClampMin — even at maximum release-note distance (distance=1.0),
// decay fraction is clamped to DefaultDecayClampMin (0.1), not 0.0.
func TestProcessAgent_DecayClampMin(t *testing.T) {
	agent := domain.AgentDefinition{
		ID:         "agent-4",
		SourceHash: "hash-new2",
	}
	registry := newTestRegistry(agent)
	registry.manifests["agent-4"] = &domain.AgentManifest{
		ReleaseNotes: "Complete rewrite",
	}

	embedder := &mockEmbedder{
		results: [][]float32{
			{0.1, 0.2},
			{0.9, 0.0},
			{0.0, 0.9},
		},
	}

	store := &mockProfileStore{
		profileToReturn: &domain.AgentProfile{
			AgentID:     "agent-4",
			SourceHash:  "hash-old2",
			SuccessRate: 1.0,
			TrustScore:  1.0,
		},
		embeddingDistFn: func(_, _ []float32) float64 { return 1.0 },
	}
	updater := &mockUpdater{}

	w := newWorker(registry, embedder, store, updater)
	if err := w.processAgent(context.Background(), agent); err != nil {
		t.Fatalf("processAgent returned error: %v", err)
	}

	if len(store.savedProfiles) != 1 {
		t.Fatalf("expected 1 saved profile, got %d", len(store.savedProfiles))
	}
	got := store.savedProfiles[0].Profile

	// decay = clamp(1.0 - 1.0, 0.1, 1.0) = 0.1
	const wantSuccessRate = DefaultDecayClampMin
	if abs64(got.SuccessRate-wantSuccessRate) > 1e-9 {
		t.Errorf("expected clamped SuccessRate %.4f, got %.4f", wantSuccessRate, got.SuccessRate)
	}
}

// Test 9: No prior profile (new agent) — seeded SuccessRate = 0.0, TrustScore = 0.0.
func TestProcessAgent_NewAgent_ColdStartFloor(t *testing.T) {
	agent := domain.AgentDefinition{
		ID:         "agent-5",
		SourceHash: "hash-first",
	}
	registry := newTestRegistry(agent)
	embedder := &mockEmbedder{results: [][]float32{{0.1, 0.2}}}
	store := &mockProfileStore{profileToReturn: nil}
	updater := &mockUpdater{}

	w := newWorker(registry, embedder, store, updater)
	if err := w.processAgent(context.Background(), agent); err != nil {
		t.Fatalf("processAgent returned error: %v", err)
	}

	if len(store.savedProfiles) != 1 {
		t.Fatalf("expected 1 saved profile, got %d", len(store.savedProfiles))
	}
	got := store.savedProfiles[0].Profile

	if got.SuccessRate != 0.0 {
		t.Errorf("expected cold-start SuccessRate 0.0, got %.4f", got.SuccessRate)
	}
	if got.TrustScore != 0.0 {
		t.Errorf("expected cold-start TrustScore 0.0, got %.4f", got.TrustScore)
	}
}

// Test 10: processAgent with a prior profile (TrustScore=0.8) must forward
// ConfidenceHint=0.8 in every ProposalRequest sent to the agent.
func TestProcessAgent_InterviewSetsConfidenceHintFromPriorProfile(t *testing.T) {
	agent := domain.AgentDefinition{
		ID:          "agent-interview-hint",
		SourceHash:  "hash-iw-001",
		Provisional: true,
	}
	registry := newTestRegistry(agent)
	embedder := &mockEmbedder{results: [][]float32{{0.1, 0.2}}}
	store := &mockProfileStore{
		profileToReturn: &domain.AgentProfile{
			AgentID:    "agent-interview-hint",
			SourceHash: "hash-iw-001",
			TrustScore: 0.8,
		},
	}
	updater := &mockUpdater{}
	requester := &mockInterviewRequester{}

	w := NewInterviewWorkerWithRequester(registry, embedder, store, updater, requester)
	if err := w.processAgent(context.Background(), agent); err != nil {
		t.Fatalf("processAgent returned error: %v", err)
	}

	if len(requester.capturedRequests) == 0 {
		t.Fatal("expected at least one RequestProposalFrom call during interview, got none")
	}
	for i, req := range requester.capturedRequests {
		if req.ConfidenceHint != float32(0.8) {
			t.Errorf("request[%d]: expected ConfidenceHint=0.8, got %f", i, req.ConfidenceHint)
		}
	}
}

// Test 11: processAgent with no prior profile (cold-start) must forward
// ConfidenceHint=0.0 in every ProposalRequest.
func TestProcessAgent_InterviewColdStartZeroHint(t *testing.T) {
	agent := domain.AgentDefinition{
		ID:          "agent-interview-cold",
		SourceHash:  "hash-iw-002",
		Provisional: true,
	}
	registry := newTestRegistry(agent)
	embedder := &mockEmbedder{results: [][]float32{{0.1, 0.2}}}
	store := &mockProfileStore{profileToReturn: nil}
	updater := &mockUpdater{}
	requester := &mockInterviewRequester{}

	w := NewInterviewWorkerWithRequester(registry, embedder, store, updater, requester)
	if err := w.processAgent(context.Background(), agent); err != nil {
		t.Fatalf("processAgent returned error: %v", err)
	}

	if len(requester.capturedRequests) == 0 {
		t.Fatal("expected at least one RequestProposalFrom call during interview, got none")
	}
	for i, req := range requester.capturedRequests {
		if req.ConfidenceHint != float32(0.0) {
			t.Errorf("request[%d]: expected ConfidenceHint=0.0 for cold-start, got %f", i, req.ConfidenceHint)
		}
	}
}

// Test 12: processAgent with nil Requester must not panic and must still complete.
func TestProcessAgent_NilRequester_NoCallsMade(t *testing.T) {
	agent := domain.AgentDefinition{
		ID:         "agent-interview-norequester",
		SourceHash: "hash-iw-003",
	}
	registry := newTestRegistry(agent)
	embedder := &mockEmbedder{results: [][]float32{{0.1, 0.2}}}
	store := &mockProfileStore{}
	updater := &mockUpdater{}

	w := newWorker(registry, embedder, store, updater)
	if err := w.processAgent(context.Background(), agent); err != nil {
		t.Fatalf("processAgent returned error with nil Requester: %v", err)
	}

	if len(store.savedProfiles) != 1 {
		t.Fatalf("expected 1 saved profile even with nil Requester, got %d", len(store.savedProfiles))
	}
}

// Test 13: SaveProfile is called with the card embedding for a RuntimeA2A agent.
func TestProcessAgent_A2A_SavesCardEmbedding(t *testing.T) {
	agent := domain.AgentDefinition{
		ID:          "a2a-agent-1",
		SourceHash:  "card-hash-1",
		Runtime:     domain.RuntimeA2A,
		A2AEndpoint: "http://a2a.example.com",
		Provisional: true,
	}
	registry := newTestRegistry(agent)

	card := &domain.AgentCard{
		Description: "I can search the web",
		Skills: []domain.A2ASkill{
			{Description: "web search"},
			{Description: "document retrieval"},
		},
	}
	fetcher := &mockCardFetcher{card: card}

	embedder := &mockEmbedder{results: [][]float32{{0.1, 0.2, 0.3}}}
	store := &mockProfileStore{}
	updater := &mockUpdater{}

	w := newWorker(registry, embedder, store, updater)
	w.CardFetcher = fetcher

	if err := w.processAgent(context.Background(), agent); err != nil {
		t.Fatalf("processAgent returned error: %v", err)
	}

	if len(store.savedProfiles) != 1 {
		t.Fatalf("expected SaveProfile called once, got %d", len(store.savedProfiles))
	}
	saved := store.savedProfiles[0]
	if saved.AgentID != agent.ID {
		t.Errorf("expected agentID %q, got %q", agent.ID, saved.AgentID)
	}
	if len(saved.Embedding) == 0 {
		t.Error("expected non-empty embedding")
	}
}

// Test 14: buildScenarios is NOT called for RuntimeA2A agents — verified by
// embedding call count (exactly 1 call for card text, not 1+ for scenarios).
func TestProcessAgent_A2A_DoesNotCallBuildScenarios(t *testing.T) {
	agent := domain.AgentDefinition{
		ID:          "a2a-agent-2",
		SourceHash:  "card-hash-2",
		Runtime:     domain.RuntimeA2A,
		A2AEndpoint: "http://a2a.example.com",
		Provisional: true,
	}
	registry := newTestRegistry(agent)
	registry.manifests[agent.ID] = &domain.AgentManifest{
		Tools: []string{"tool1", "tool2", "tool3"},
	}

	card := &domain.AgentCard{Description: "A2A only card", Skills: []domain.A2ASkill{}}
	fetcher := &mockCardFetcher{card: card}

	cardEmbedding := []float32{0.99, 0.01}
	embedder := &mockEmbedder{results: [][]float32{cardEmbedding}}
	store := &mockProfileStore{}
	updater := &mockUpdater{}

	w := newWorker(registry, embedder, store, updater)
	w.CardFetcher = fetcher

	if err := w.processAgent(context.Background(), agent); err != nil {
		t.Fatalf("processAgent returned error: %v", err)
	}

	if embedder.callIdx != 1 {
		t.Errorf("expected exactly 1 Embed call (card text), got %d — buildScenarios may have been called", embedder.callIdx)
	}
	if len(store.savedProfiles) != 1 {
		t.Fatalf("expected 1 saved profile, got %d", len(store.savedProfiles))
	}
}

// Test 15: computeDecay is applied when a prior profile exists for the A2A agent.
func TestProcessAgent_A2A_ComputeDecayApplied(t *testing.T) {
	agent := domain.AgentDefinition{
		ID:          "a2a-agent-3",
		SourceHash:  "card-hash-3",
		Runtime:     domain.RuntimeA2A,
		A2AEndpoint: "http://a2a.example.com",
		Provisional: true,
	}
	registry := newTestRegistry(agent)

	card := &domain.AgentCard{
		Description: "Updated A2A agent",
		Skills:      []domain.A2ASkill{{Description: "new skill"}},
	}
	fetcher := &mockCardFetcher{card: card}

	embedder := &mockEmbedder{
		results: [][]float32{
			{0.5, 0.5}, // card text (call 0)
			{1.0, 0.0}, // release notes embed (computeDecay, call 1)
			{0.0, 1.0}, // prior source hash proxy (computeDecay, call 2)
		},
	}

	store := &mockProfileStore{
		profileToReturn: &domain.AgentProfile{
			AgentID:     agent.ID,
			SourceHash:  "card-hash-old",
			SuccessRate: 0.8,
			TrustScore:  0.6,
		},
		embeddingDistFn: func(_, _ []float32) float64 { return 0.5 },
	}
	updater := &mockUpdater{}

	w := newWorker(registry, embedder, store, updater)
	w.CardFetcher = fetcher

	if err := w.processAgent(context.Background(), agent); err != nil {
		t.Fatalf("processAgent returned error: %v", err)
	}

	if len(store.savedProfiles) != 1 {
		t.Fatalf("expected 1 saved profile, got %d", len(store.savedProfiles))
	}
	got := store.savedProfiles[0].Profile

	const wantSuccessRate = 0.4 // 0.8 * 0.5
	if abs64(got.SuccessRate-wantSuccessRate) > 1e-9 {
		t.Errorf("expected SuccessRate %.4f (prior*decay), got %.4f", wantSuccessRate, got.SuccessRate)
	}
	const wantTrustScore = 0.3 // 0.6 * 0.5
	if abs64(got.TrustScore-wantTrustScore) > 1e-9 {
		t.Errorf("expected TrustScore %.4f (prior*decay), got %.4f", wantTrustScore, got.TrustScore)
	}
}
