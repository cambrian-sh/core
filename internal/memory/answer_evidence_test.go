package memory

import (
	"context"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// recordingCitedPlanner records the passages the cited synthesis was handed.
type recordingCitedPlanner struct {
	fakePlanner
	texts []string
}

func (c *recordingCitedPlanner) SynthesizeCited(_ context.Context, _ string, texts []string) (string, string, error) {
	c.texts = append([]string(nil), texts...)
	return "answer", "grounded [1] [2]", nil
}

// ENG-223: the AnswerMemory pool filters narrow the evidence BEFORE the cited
// synthesis — the answer is never built from a row the caller filtered out, and
// the returned evidence is exactly what was synthesized, so [n] ↔ evidence[n-1].
func TestAnswer_EvidenceFilterAppliesBeforeSynthesis(t *testing.T) {
	t.Parallel()
	store := &scopeApplyingStore{docs: []domain.Document{
		{ID: "a1", Text: "alpha one", Metadata: map[string]interface{}{"source": "a"}},
		{ID: "b1", Text: "beta one", Metadata: map[string]interface{}{"source": "b"}},
		{ID: "a2", Text: "alpha two", Metadata: map[string]interface{}{"source": "a"}},
	}}
	planner := &recordingCitedPlanner{}
	q := NewQueryService(&fakeEmbedder{}, store, &recordingAuthorizer{})
	q.EnableAgenticRetrieval(planner, 1)

	ctx := WithAnswerEvidence(context.Background(), AnswerEvidence{
		Keep: func(r domain.SearchResult) bool { return r.Document.Metadata["source"] != "b" },
	})
	_, _, evidence, err := q.AnswerSystem(ctx, "alpha?")
	if err != nil {
		t.Fatalf("AnswerSystem: %v", err)
	}
	if len(planner.texts) == 0 {
		t.Fatal("synthesis was never called")
	}
	for _, txt := range planner.texts {
		if strings.Contains(txt, "beta") {
			t.Fatalf("filtered-out row reached the synthesis: %q", planner.texts)
		}
	}
	if len(evidence) != len(planner.texts) {
		t.Fatalf("evidence %d rows, synthesized %d passages: [n] would not resolve to evidence[n-1]", len(evidence), len(planner.texts))
	}
	for i, r := range evidence {
		if r.Document.ID == "b1" || !strings.Contains(planner.texts[i], r.Document.Text) {
			t.Fatalf("evidence[%d] = %s does not match passage %q", i, r.Document.ID, planner.texts[i])
		}
	}

	// The cap applies after the filter.
	capped := WithAnswerEvidence(context.Background(), AnswerEvidence{Max: 1})
	if _, _, ev, _ := q.AnswerSystem(capped, "alpha?"); len(ev) != 1 {
		t.Fatalf("Max 1: evidence = %d rows", len(ev))
	}
}
