package memory

import (
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// TestLaneClassification pins which document types count as experience. Getting this
// wrong is silent: a misclassified type simply stops being protected, and the only
// symptom is corpus recall quietly degrading once experience accumulates.
func TestLaneClassification(t *testing.T) {
	experiential := []string{
		domain.DocTypeMnemonicScene, domain.DocTypeMnemonicAction,
		domain.DocTypeMnemonicEntity, domain.DocTypeEpisodicMemory,
		domain.DocTypeNegativeEdge,
	}
	for _, dt := range experiential {
		if !isExperientialDoc(domain.Document{DocumentType: dt}) {
			t.Errorf("%s must be classified as experience", dt)
		}
	}
	// The corpus: what an operator deliberately ingested.
	for _, dt := range []string{domain.DocTypeMnemonicFact, domain.DocTypeDocSection, ""} {
		if isExperientialDoc(domain.Document{DocumentType: dt}) {
			t.Errorf("%s must be classified as corpus", dt)
		}
	}
	// An agent-written fact parented to an episode is experience despite its type.
	if !isExperientialDoc(domain.Document{DocumentType: domain.DocTypeMnemonicFact, ExperienceID: "exp-1"}) {
		t.Error("a fact parented to an episode is experience")
	}
}

// A knowledge query asks in the corpus lane; action/precedent queries ask in the
// experience lane. If this inverts, the guard protects exactly the wrong side.
func TestLaneOfQuery(t *testing.T) {
	if laneIsExperiential(domain.DocTypeMnemonicFact) {
		t.Error("a fact query asks in the CORPUS lane")
	}
	for _, dt := range []string{domain.DocTypeMnemonicAction, domain.DocTypeMnemonicScene, domain.DocTypeNegativeEdge} {
		if !laneIsExperiential(dt) {
			t.Errorf("%s query asks in the EXPERIENCE lane", dt)
		}
	}
}

// TestLaneGuard_ExperienceNeverEvictsCorpusPrimary is defect 4, as a test.
//
// The scenario that motivates it: a noisy store where experiential rows carry the 0.5
// injection floor and outscore a genuine but low-cosine corpus hit. Before the lane
// dimension, sorting by blended score alone let them take the window.
func TestLaneGuard_ExperienceNeverEvictsCorpusPrimary(t *testing.T) {
	topK := 2
	results := []domain.SearchResult{
		{Document: domain.Document{ID: "scene-1", DocumentType: domain.DocTypeMnemonicScene}, Score: 0.9},
		{Document: domain.Document{ID: "scene-2", DocumentType: domain.DocTypeMnemonicScene}, Score: 0.8},
		{Document: domain.Document{ID: "corpus-1", DocumentType: domain.DocTypeMnemonicFact}, Score: 0.3},
	}
	// All three are PRIMARY, so the primary/injected split alone cannot decide this —
	// only the lane dimension can. (An earlier version of this test marked just the
	// corpus row primary, and passed even with the lane logic removed.)
	primaryIDs := map[string]bool{"corpus-1": true, "scene-1": true, "scene-2": true}
	got := assembleLanes(results, primaryIDs, topK, laneIsExperiential(domain.DocTypeMnemonicFact), nil)

	if len(got) == 0 || got[0].Document.ID != "corpus-1" {
		t.Fatalf("the corpus primary must claim the first slot even at a lower score, got %+v", laneIDs(got))
	}
	if len(got) != topK {
		t.Errorf("remaining slots should still be filled by experience, got %v", laneIDs(got))
	}
}

// The mirror: a precedent query must not have its precedents pushed out by corpus.
func TestLaneGuard_CorpusNeverEvictsPrecedent(t *testing.T) {
	results := []domain.SearchResult{
		{Document: domain.Document{ID: "corpus-1", DocumentType: domain.DocTypeMnemonicFact}, Score: 0.95},
		{Document: domain.Document{ID: "scene-1", DocumentType: domain.DocTypeMnemonicScene}, Score: 0.2},
	}
	primaryIDs := map[string]bool{"corpus-1": true, "scene-1": true}
	got := assembleLanes(results, primaryIDs, 1, laneIsExperiential(domain.DocTypeMnemonicScene), nil)
	if len(got) != 1 || got[0].Document.ID != "scene-1" {
		t.Errorf("a precedent query must keep its precedent, got %v", laneIDs(got))
	}
}

func laneIDs(rs []domain.SearchResult) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Document.ID)
	}
	return out
}
