package interview

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// recordingEmbedder records every text it is asked to embed.
type recordingEmbedder struct {
	texts []string
	inner mockEmbedder
}

func (e *recordingEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	e.texts = append(e.texts, text)
	return e.inner.Embed(ctx, text)
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// ADR-0002 D2 / ADR-0006 D5: inherited merit decays by the embedding distance
// between the OLD and NEW release notes. The worker used to embed the prior
// profile's SourceHash as the "old" text
// (T-20261031-interview-merit-decay-compares-release-notes).
func TestComputeDecay_ComparesReleaseNotesNeverTheHash(t *testing.T) {
	agent := domain.AgentDefinition{ID: "agent-notes", SourceHash: "hash-new", Provisional: true}
	registry := newTestRegistry(agent)
	registry.manifests["agent-notes"] = &domain.AgentManifest{ReleaseNotes: "v2: adds streaming"}

	emb := &recordingEmbedder{inner: mockEmbedder{results: [][]float32{{0.1, 0.2}, {0.5, 0.5}, {0.4, 0.6}}}}
	store := &mockProfileStore{
		profileToReturn: &domain.AgentProfile{
			AgentID:      "agent-notes",
			SourceHash:   "hash-old",
			ReleaseNotes: "v1: initial release",
			SuccessRate:  0.8,
			TrustScore:   0.6,
		},
		embeddingDistFn: func(_, _ []float32) float64 { return 0.25 },
	}

	w := NewInterviewWorker(registry, emb, store, &mockUpdater{})
	if err := w.processAgent(context.Background(), agent); err != nil {
		t.Fatalf("processAgent: %v", err)
	}

	for _, txt := range emb.texts {
		if txt == "hash-old" || txt == "hash-new" {
			t.Fatalf("a source hash was embedded as release-note text: %q (all: %q)", txt, emb.texts)
		}
	}
	if !contains(emb.texts, "v1: initial release") || !contains(emb.texts, "v2: adds streaming") {
		t.Fatalf("both versions' release notes must be embedded for the decay, embedded: %q", emb.texts)
	}

	if len(store.savedProfiles) != 1 {
		t.Fatalf("expected 1 saved profile, got %d", len(store.savedProfiles))
	}
	got := store.savedProfiles[0].Profile
	if got.ReleaseNotes != "v2: adds streaming" {
		t.Fatalf("the new profile must persist its release notes for the NEXT version's decay, got %q", got.ReleaseNotes)
	}
	// decay = clamp(1 - 0.25) = 0.75
	if abs64(got.SuccessRate-0.8*0.75) > 1e-9 || abs64(got.TrustScore-0.6*0.75) > 1e-9 {
		t.Fatalf("seeded merit should be prior × 0.75, got success=%.4f trust=%.4f", got.SuccessRate, got.TrustScore)
	}
}

// A prior profile written before release notes were persisted has nothing to
// compare against: the conservative clamp floor applies, and the hash is still
// never embedded.
func TestComputeDecay_PriorWithoutNotesFallsToClampFloor(t *testing.T) {
	agent := domain.AgentDefinition{ID: "agent-legacy", SourceHash: "hash-new", Provisional: true}
	registry := newTestRegistry(agent)
	registry.manifests["agent-legacy"] = &domain.AgentManifest{ReleaseNotes: "v2"}

	emb := &recordingEmbedder{inner: mockEmbedder{}}
	store := &mockProfileStore{
		profileToReturn: &domain.AgentProfile{AgentID: "agent-legacy", SourceHash: "hash-old", SuccessRate: 1.0, TrustScore: 1.0},
		embeddingDistFn: func(_, _ []float32) float64 { return 0.0 },
	}
	w := NewInterviewWorker(registry, emb, store, &mockUpdater{})
	if err := w.processAgent(context.Background(), agent); err != nil {
		t.Fatalf("processAgent: %v", err)
	}
	if contains(emb.texts, "hash-old") {
		t.Fatalf("the prior source hash must never be embedded, embedded: %q", emb.texts)
	}
	got := store.savedProfiles[0].Profile
	if abs64(got.SuccessRate-DefaultDecayClampMin) > 1e-9 {
		t.Fatalf("without prior notes the decay must be the clamp floor %.2f, got %.4f", DefaultDecayClampMin, got.SuccessRate)
	}
}

// T-20261101-interview-reinterview-never-finds-the-previous: the prior profile
// is keyed by hash-old while the agent now carries hash-new. GetProfile(hash-new)
// finds nothing; the worker must still seed merit from the previous version
// (it used to seed 0).
func TestInterview_VersionChangeSeedsMeritFromPreviousHash(t *testing.T) {
	agent := domain.AgentDefinition{ID: "agent-v2", SourceHash: "hash-new", Provisional: true}
	registry := newTestRegistry(agent)
	registry.manifests["agent-v2"] = &domain.AgentManifest{ReleaseNotes: "v2: faster"}
	store := &mockProfileStore{
		profileToReturn: nil, // nothing under hash-new
		previousToReturn: &domain.AgentProfile{
			AgentID: "agent-v2", SourceHash: "hash-old", ReleaseNotes: "v1",
			SuccessRate: 0.8, TrustScore: 0.6,
		},
		embeddingDistFn: func(_, _ []float32) float64 { return 0.25 },
	}
	w := NewInterviewWorker(registry, &mockEmbedder{}, store, &mockUpdater{})
	if err := w.processAgent(context.Background(), agent); err != nil {
		t.Fatalf("processAgent: %v", err)
	}
	got := store.savedProfiles[0].Profile
	if got.SuccessRate == 0 || got.TrustScore == 0 {
		t.Fatalf("version change seeded no merit (the bug): %+v", got)
	}
	if abs64(got.SuccessRate-0.8*0.75) > 1e-9 || abs64(got.TrustScore-0.6*0.75) > 1e-9 {
		t.Fatalf("seeded merit should be previous × 0.75, got success=%.4f trust=%.4f", got.SuccessRate, got.TrustScore)
	}
}

// The same-hash profile, when present, wins over another version's.
func TestInterview_SameHashProfileWinsOverPrevious(t *testing.T) {
	agent := domain.AgentDefinition{ID: "a", SourceHash: "h1"}
	store := &mockProfileStore{
		profileToReturn:  &domain.AgentProfile{AgentID: "a", SourceHash: "h1", SuccessRate: 0.9},
		previousToReturn: &domain.AgentProfile{AgentID: "a", SourceHash: "h0", SuccessRate: 0.1},
	}
	w := NewInterviewWorker(newTestRegistry(agent), &mockEmbedder{}, store, &mockUpdater{})
	if p := w.priorProfile(context.Background(), agent); p == nil || p.SourceHash != "h1" {
		t.Fatalf("prior = %+v, want the same-hash profile", p)
	}
}
