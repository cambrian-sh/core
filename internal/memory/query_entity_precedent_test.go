package memory

import (
	"context"
	"strings"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// programmableStore lets a test script GetByID / Search / QueryByMetadata independently.
type programmableStore struct {
	fakeVectorStore
	byID     map[string]*domain.Document
	searched []domain.SearchResult
	byMeta   []domain.Document
}

func (p *programmableStore) GetByID(_ context.Context, id string) (*domain.Document, error) {
	if d, ok := p.byID[id]; ok {
		return d, nil
	}
	return nil, nil
}
func (p *programmableStore) Search(_ context.Context, _ []float32, _ domain.SearchOptions) ([]domain.SearchResult, error) {
	return p.searched, nil
}
func (p *programmableStore) QueryByMetadata(_ context.Context, _ map[string]string, _ int) ([]domain.Document, error) {
	return p.byMeta, nil
}

// ADR-0049 Issue 012: exact lookup resolves a canonical kind:id to ONE entity, rendering
// the materialized current state (a deleted file reads exists=false) + the most-recent
// engaging scene link. This is reconstruction, not semantic search.
func TestSearchEntities_ReconstructsCurrentState(t *testing.T) {
	store := &programmableStore{byID: map[string]*domain.Document{
		"file:docs/a.md": {
			ID:           "file:docs/a.md",
			DocumentType: domain.DocTypeMnemonicEntity,
			Metadata: map[string]interface{}{
				"fields":     `{"exists":{"value":false,"seq":3},"content_ref":{"value":"sha256:abc","seq":2}}`,
				"last_scene": "scene-p7",
			},
		},
	}}
	q := NewQueryService(&fakeEmbedder{}, store)

	res, err := q.SearchEntities(context.Background(), "file:docs/a.md", "agent-a")
	if err != nil || len(res) != 1 {
		t.Fatalf("expected one entity record; got %d err=%v", len(res), err)
	}
	text := res[0].Document.Text
	if !strings.Contains(text, "exists=false") {
		t.Errorf("reconstruction must reflect the superseded (deleted) state; got %q", text)
	}
	if !strings.Contains(text, "content_ref=sha256:abc") {
		t.Errorf("reconstruction must surface the current content_ref; got %q", text)
	}
	if !strings.Contains(text, "last_scene=scene-p7") {
		t.Errorf("reconstruction must link the most-recent engaging scene; got %q", text)
	}
}

func TestSearchEntities_UnknownReturnsEmpty(t *testing.T) {
	store := &programmableStore{byID: map[string]*domain.Document{}}
	q := NewQueryService(&fakeEmbedder{}, store)
	res, err := q.SearchEntities(context.Background(), "file:nope.md", "agent-a")
	if err != nil || len(res) != 0 {
		t.Errorf("unknown entity must return empty (no record), no error; got %d err=%v", len(res), err)
	}
}

// ADR-0049 D11/Issue 014: the precedent pull lane returns failure-weighted transitions;
// below the floor it returns "no precedent" (empty), never a fabricated analogy.
func TestSearchPrecedents_FailureWeightedTransitions(t *testing.T) {
	store := &programmableStore{
		searched: []domain.SearchResult{
			sceneResult("scene-good", "goal: ship | engages: 1 file", "success", 0.9),
			sceneResult("scene-bad", "goal: ship | engages: 1 file", "failure", 0.7),
		},
		byMeta: []domain.Document{actionDoc("write_file → ok", "t1")},
	}
	q := NewQueryService(&fakeEmbedder{}, store)

	res, err := q.SearchPrecedents(context.Background(), "ship the thing", "agent-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("expected 2 precedents; got %d", len(res))
	}
	// Failure must rank first (most decision-relevant), even though its similarity is lower.
	if res[0].Document.ID != "scene-bad" {
		t.Errorf("failure precedent must rank first; got order %q, %q", res[0].Document.ID, res[1].Document.ID)
	}
	if !strings.Contains(res[0].Document.Text, "OUTCOME: failure") || !strings.Contains(res[0].Document.Text, "write_file → ok") {
		t.Errorf("a precedent must carry the transition (outcome + action path); got %q", res[0].Document.Text)
	}
}

func TestSearchPrecedents_NoScenesNoPrecedent(t *testing.T) {
	store := &programmableStore{searched: nil} // below floor / cold corpus → no scenes
	q := NewQueryService(&fakeEmbedder{}, store)
	res, err := q.SearchPrecedents(context.Background(), "novel situation", "agent-a")
	if err != nil || len(res) != 0 {
		t.Errorf("no scenes must yield no precedent (not fabricated); got %d err=%v", len(res), err)
	}
}

// ADR-0049 Issue 013: PrimeForPlanning's enrichment carries the precedent lane.
func TestPrimeForPlanning_IncludesPrecedentLane(t *testing.T) {
	store := &programmableStore{
		searched: []domain.SearchResult{
			sceneResult("scene-bad", "goal: deploy | engages: 1 web resource", "failure", 0.8),
		},
		byMeta: []domain.Document{actionDoc("deploy → err", "t1")},
	}
	w := NewWorkspaceStage(store, &fakeEmbedder{}, nil, 5, 5, 0.0, false, 0.0)

	enr, err := w.PrimeForPlanning(context.Background(), "deploy the service")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(enr.Precedents) != 1 || enr.Precedents[0].Outcome != "failure" {
		t.Fatalf("planning enrichment must include a failure precedent; got %+v", enr.Precedents)
	}
	if len(enr.Precedents[0].Actions) != 1 || enr.Precedents[0].Actions[0] != "deploy → err" {
		t.Errorf("the precedent must carry its action path; got %v", enr.Precedents[0].Actions)
	}
}
