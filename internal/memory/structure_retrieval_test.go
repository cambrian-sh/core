package memory

import (
	"context"
	"reflect"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ENG-2108: section scoping lifts matched chunks above every other result, so it
// may fire only when the query names a place in a document.

var testCues = map[string]bool{"section": true, "chapter": true, "bölüm": true}

func TestExtractSectionTerms_ContentWordsNeedACue(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  []string
	}{
		// Ordinary questions that share a word with some heading: no scoping.
		{"What is the readiness canary codeword for project Larkspur?", nil},
		{"Who manages the Antalya warehouse?", nil},
		// A cue word names a heading by its title.
		{"What does the Recall Readiness section say?", []string{"recall", "readiness"}},
		{"Summarise the genetics chapter", []string{"summarise", "genetics"}},
		// A cue in the deployment's own language (config, not code).
		{"Hazırlık bölüm ne diyor?", []string{"hazırlık", "diyor"}},
		// A hierarchical number is a locator on its own, and the only term then.
		{"what does 3.2 say about sampling", []string{"3.2"}},
		{"Section 4.10.1 on sampling", []string{"4.10.1"}},
	} {
		if got := extractSectionTerms(tc.query, testCues); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("extractSectionTerms(%q) = %q, want %q", tc.query, got, tc.want)
		}
	}
}

func TestExtractSectionTerms_NoCuesMeansNumbersOnly(t *testing.T) {
	if got := extractSectionTerms("What does the Recall Readiness section say?", nil); got != nil {
		t.Errorf("with no cues configured, content words must not scope: got %q", got)
	}
	if got := extractSectionTerms("what does 3.2 say", nil); !reflect.DeepEqual(got, []string{"3.2"}) {
		t.Errorf("numbers scope without cues: got %q", got)
	}
}

type recordingSectionStore struct {
	ids   []string
	terms [][]string
}

func (s *recordingSectionStore) ChunksInMatchingSections(_ context.Context, terms []string, _ int) ([]string, error) {
	s.terms = append(s.terms, terms)
	return s.ids, nil
}

func TestApplySectionConstraint_UncuedQueryKeepsRelevanceOrder(t *testing.T) {
	store := &recordingSectionStore{ids: []string{"sop-related"}}
	q := &QueryService{}
	q.EnableSectionScopedRetrieval(store, []string{" Section ", "CHAPTER", ""})
	pool := []domain.SearchResult{
		{Document: domain.Document{ID: "memo"}, Score: 0.83},
		{Document: domain.Document{ID: "sop-related"}, Score: 0.41},
	}
	got := q.applySectionConstraint(context.Background(), pool, "What is the readiness canary codeword?", nil)
	if len(store.terms) != 0 {
		t.Errorf("an uncued question must not reach the section store; looked up %q", store.terms)
	}
	if got[0].Document.ID != "memo" || got[0].Score != 0.83 || got[1].Score != 0.41 {
		t.Errorf("order/scores changed: %+v", got)
	}

	// Cue words are normalised (trimmed, lowercased): a cued query is promoted.
	got = q.applySectionConstraint(context.Background(), pool, "What does the readiness section say?", nil)
	if len(store.terms) != 1 || !reflect.DeepEqual(store.terms[0], []string{"readiness"}) {
		t.Fatalf("cued lookup terms = %q, want [readiness]", store.terms)
	}
	if got[0].Document.ID != "sop-related" || got[0].Score < 1 {
		t.Errorf("a cued query should promote the section's chunk: %+v", got)
	}
}
