package interview

import (
	"context"
	"testing"
	"time"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// Test 16: processAgent for a TraitTool agent must call Store.SaveProfile exactly
// once with TrustScore=1.0, SuccessRate=1.0, and NetworkLatencyMedianMs=5.
func TestProcessAgent_TraitTool_StaticMeritSaved(t *testing.T) {
	agent := domain.AgentDefinition{
		ID:          "calc-agent",
		SourceHash:  "tool-hash-1",
		Trait:       domain.TraitTool,
		Provisional: true,
	}
	reg := newMockManifestReader(nil)
	embedder := &trackingEmbedder{inner: &mockEmbedder{results: [][]float32{{0.1}}}}
	store := &mockProfileStore{}
	updater := &mockUpdater{}

	w := NewInterviewWorker(reg, embedder, store, updater)
	if err := w.processAgent(context.Background(), agent); err != nil {
		t.Fatalf("processAgent returned error: %v", err)
	}

	if len(store.savedProfiles) != 1 {
		t.Fatalf("expected SaveProfile called once, got %d", len(store.savedProfiles))
	}
	got := store.savedProfiles[0].Profile

	if got.TrustScore != 1.0 {
		t.Errorf("expected TrustScore=1.0, got %f", got.TrustScore)
	}
	if got.SuccessRate != 1.0 {
		t.Errorf("expected SuccessRate=1.0, got %f", got.SuccessRate)
	}
	const wantLatencyMs = 5
	if got.NetworkLatencyMedianMs != wantLatencyMs {
		t.Errorf("expected NetworkLatencyMedianMs=%d, got %d", wantLatencyMs, got.NetworkLatencyMedianMs)
	}
}

// Test 17: processAgent for a TraitTool agent MUST call Embedder.Embed with the
// agent description so the agent participates in cosine-similarity clustering.
func TestProcessAgent_TraitTool_EmbedsCalled(t *testing.T) {
	agent := domain.AgentDefinition{
		ID:          "calc-agent-2",
		SourceHash:  "tool-hash-2",
		Trait:       domain.TraitTool,
		Description: "Deterministic calculator tool",
		Provisional: true,
	}
	reg := newMockManifestReader(nil)
	embedder := &trackingEmbedder{inner: &mockEmbedder{results: [][]float32{{0.1, 0.2}}}}
	store := &mockProfileStore{}
	updater := &mockUpdater{}

	w := NewInterviewWorker(reg, embedder, store, updater)
	if err := w.processAgent(context.Background(), agent); err != nil {
		t.Fatalf("processAgent returned error: %v", err)
	}

	if !embedder.called {
		t.Error("expected Embedder.Embed to be called for TraitTool agent (description embedding), but it was not")
	}
}

// Test 18: processAgent for a TraitTool agent must call Updater.SetProvisional(agentID, false).
func TestProcessAgent_TraitTool_SetProvisionalFalse(t *testing.T) {
	agent := domain.AgentDefinition{
		ID:          "calc-agent-3",
		SourceHash:  "tool-hash-3",
		Trait:       domain.TraitTool,
		Provisional: true,
	}
	reg := newMockManifestReader(nil)
	embedder := &trackingEmbedder{inner: &mockEmbedder{results: [][]float32{{0.1}}}}
	store := &mockProfileStore{}
	updater := &mockUpdater{}

	w := NewInterviewWorker(reg, embedder, store, updater)
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

// Test 19: processAgent for a TraitTool agent must pass a NON-EMPTY embedding
// vector to SaveProfile so the agent participates in capability clustering.
func TestProcessAgent_TraitTool_NonEmptyEmbeddingVector(t *testing.T) {
	agent := domain.AgentDefinition{
		ID:          "calc-agent-4",
		SourceHash:  "tool-hash-4",
		Trait:       domain.TraitTool,
		Description: "Deterministic calculator",
		Provisional: true,
	}
	reg := newMockManifestReader(nil)
	embedder := &trackingEmbedder{inner: &mockEmbedder{results: [][]float32{{0.5, 0.6}}}}
	store := &mockProfileStore{}
	updater := &mockUpdater{}

	w := NewInterviewWorker(reg, embedder, store, updater)
	if err := w.processAgent(context.Background(), agent); err != nil {
		t.Fatalf("processAgent returned error: %v", err)
	}

	if len(store.savedProfiles) != 1 {
		t.Fatalf("expected 1 saved profile, got %d", len(store.savedProfiles))
	}
	emb := store.savedProfiles[0].Embedding
	if len(emb) == 0 {
		t.Error("expected non-empty embedding for TraitTool agent, got zero-length vector")
	}
}

// Test 20: processAgent for a TraitTool agent must NOT call GetJudicialRecords
// or buildScenarios — exactly one Embed call (description), not scenario-driven.
func TestProcessAgent_TraitTool_NoScenarioCalls(t *testing.T) {
	agent := domain.AgentDefinition{
		ID:          "calc-agent-5",
		SourceHash:  "tool-hash-5",
		Trait:       domain.TraitTool,
		Description: "Calculator",
		Provisional: true,
	}
	reg := newMockManifestReader(map[string]*domain.AgentManifest{
		agent.ID: {Tools: []string{"add", "subtract", "multiply"}},
	})
	embedder := &mockEmbedder{results: [][]float32{{0.5, 0.5}}}
	store := &mockProfileStore{
		judicialRecords: []string{"some critique"},
	}
	updater := &mockUpdater{}

	w := NewInterviewWorker(reg, embedder, store, updater)
	if err := w.processAgent(context.Background(), agent); err != nil {
		t.Fatalf("processAgent returned error: %v", err)
	}

	// Exactly 1 Embed call (description only — no scenario or judicial-record calls)
	if embedder.callIdx != 1 {
		t.Errorf("expected exactly 1 Embed call for TraitTool agent, got %d", embedder.callIdx)
	}
	if len(store.savedProfiles) != 1 {
		t.Fatalf("expected 1 saved profile, got %d", len(store.savedProfiles))
	}
	got := store.savedProfiles[0].Profile
	if got.TrustScore != 1.0 || got.SuccessRate != 1.0 {
		t.Errorf("expected static Merit (1.0/1.0), got TrustScore=%f SuccessRate=%f",
			got.TrustScore, got.SuccessRate)
	}
}

// Ensure the 5ms constant is what we expect.
var _ = (5 * time.Millisecond)
