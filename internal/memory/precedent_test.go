package memory

import (
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

func sceneResult(id, projection, outcome string, score float64) domain.SearchResult {
	return domain.SearchResult{
		Document: domain.Document{
			ID:           id,
			DocumentType: domain.DocTypeMnemonicScene,
			Text:         "reconstruction text for " + id,
			Metadata:     map[string]interface{}{"projection": projection, "outcome": outcome},
		},
		Score: score,
	}
}

func actionDoc(text, ts string) domain.Document {
	return domain.Document{
		DocumentType: domain.DocTypeMnemonicAction,
		Text:         text,
		Metadata:     map[string]interface{}{"timestamp": ts},
	}
}

// ADR-0049 D11: a precedent is assembled from stored data — situation (abstracted
// projection), outcome, and the action path — never guessed.
func TestBuildPrecedent(t *testing.T) {
	scene := sceneResult("scene-p1", "goal: ship | engages: 1 file", "failure", 0.8)
	actions := []domain.Document{actionDoc("write_file → ok", "t1"), actionDoc("deploy → err", "t2")}

	p := buildPrecedent(scene, actions)
	if p.Situation != "goal: ship | engages: 1 file" {
		t.Errorf("situation must be the abstracted projection; got %q", p.Situation)
	}
	if p.Outcome != "failure" || p.Success {
		t.Errorf("outcome must come from metadata; got outcome=%q success=%v", p.Outcome, p.Success)
	}
	if len(p.Actions) != 2 || p.Actions[0] != "write_file → ok" {
		t.Errorf("the action path must be carried in order; got %v", p.Actions)
	}
	if p.Similarity != 0.8 {
		t.Errorf("similarity must carry the scene score; got %v", p.Similarity)
	}
}

// ADR-0049 D11: failures rank ahead of successes; within a group, higher similarity first.
func TestSortPrecedentsFailureFirst(t *testing.T) {
	ps := []domain.Precedent{
		{SceneID: "ok-hi", Success: true, Similarity: 0.9},
		{SceneID: "fail-lo", Success: false, Similarity: 0.5},
		{SceneID: "fail-hi", Success: false, Similarity: 0.7},
		{SceneID: "ok-lo", Success: true, Similarity: 0.4},
	}
	sortPrecedentsFailureFirst(ps)
	order := []string{ps[0].SceneID, ps[1].SceneID, ps[2].SceneID, ps[3].SceneID}
	want := []string{"fail-hi", "fail-lo", "ok-hi", "ok-lo"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("failure-weighted order wrong; got %v want %v", order, want)
		}
	}
}

// filterActionsForPlan keeps only actions, ordered by timestamp (drops the scene/other docs).
func TestFilterActionsForPlan(t *testing.T) {
	docs := []domain.Document{
		actionDoc("b", "2024-01-02T00:00:00Z"),
		{DocumentType: domain.DocTypeMnemonicScene, Text: "the scene"},
		actionDoc("a", "2024-01-01T00:00:00Z"),
	}
	got := filterActionsForPlan(docs)
	if len(got) != 2 || got[0].Text != "a" || got[1].Text != "b" {
		t.Fatalf("must keep only actions in time order; got %+v", got)
	}
}

func TestPrecedentText(t *testing.T) {
	txt := precedentText(domain.Precedent{Situation: "goal: x", Outcome: "failure", Actions: []string{"a1", "a2"}})
	if !strings.Contains(txt, "SITUATION: goal: x") || !strings.Contains(txt, "OUTCOME: failure") || !strings.Contains(txt, "a1; a2") {
		t.Errorf("transition framing must carry situation+outcome+actions; got %q", txt)
	}
}
