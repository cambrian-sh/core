package interview

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// TestIntegration_ReinterviewMeritDecay verifies that when an agent is updated
// (new SourceHash), the re-interview decay is computed correctly.
// A high embedding distance (0.8) yields decay = clamp(1-0.8, 0.1, 1.0) = 0.2.
func TestIntegration_ReinterviewMeritDecay(t *testing.T) {
	const (
		priorTrustScore  = 0.9
		priorSuccessRate = 0.8
		embedDist        = 0.8
	)

	agent := domain.AgentDefinition{
		ID:          "agent-v1",
		SourceHash:  "new-hash",
		Provisional: true,
	}
	reg := newMockManifestReader(map[string]*domain.AgentManifest{
		"agent-v1": {
			ReleaseNotes: "Major overhaul of the SQL engine",
			Tools:        []string{"sql"},
		},
	})

	embedder := &mockEmbedder{
		results: [][]float32{
			{0.1, 0.2, 0.3}, // scenarios combined text
			{1.0, 0.0, 0.0}, // new release notes
			{0.0, 1.0, 0.0}, // prior SourceHash proxy
		},
	}

	store := &mockProfileStore{
		profileToReturn: &domain.AgentProfile{
			AgentID:     "agent-v1",
			SourceHash:  "old-hash",
			TrustScore:  priorTrustScore,
			SuccessRate: priorSuccessRate,
		},
		embeddingDistFn: func(_, _ []float32) float64 { return embedDist },
	}
	updater := &mockUpdater{}

	iw := NewInterviewWorker(reg, embedder, store, updater)
	if err := iw.processAgent(context.Background(), agent); err != nil {
		t.Fatalf("processAgent error: %v", err)
	}

	if len(store.savedProfiles) != 1 {
		t.Fatalf("expected 1 saved profile, got %d", len(store.savedProfiles))
	}
	got := store.savedProfiles[0].Profile

	// decay = clamp(1 - 0.8, 0.1, 1.0) = 0.2
	expectedDecay := clamp(1.0-embedDist, DefaultDecayClampMin, DefaultDecayClampMax)
	wantTrustScore := priorTrustScore * expectedDecay
	wantSuccessRate := priorSuccessRate * expectedDecay

	if got.TrustScore >= priorTrustScore {
		t.Errorf("re-interviewed TrustScore=%.4f must be below prior TrustScore=%.4f",
			got.TrustScore, priorTrustScore)
	}
	if got.TrustScore <= 0 {
		t.Errorf("re-interviewed TrustScore=%.4f must be above cold-start floor (0.0)", got.TrustScore)
	}
	if abs64(got.TrustScore-wantTrustScore) > 1e-9 {
		t.Errorf("expected TrustScore=%.4f (prior*decay), got %.4f", wantTrustScore, got.TrustScore)
	}
	if abs64(got.SuccessRate-wantSuccessRate) > 1e-9 {
		t.Errorf("expected SuccessRate=%.4f (prior*decay), got %.4f", wantSuccessRate, got.SuccessRate)
	}
}
