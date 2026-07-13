package memory

import (
	"context"
	"fmt"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

func stepRec(id, sid string) domain.Document {
	return domain.Document{
		ID:       id,
		Text:     "step_0: a big accreted blob",
		Metadata: map[string]interface{}{"source_agent": "System", "session_id": sid},
	}
}

// The pure predicate: only the CURRENT session's auto-recorded System step
// records match; cross-session records and deliberate remember() facts do not.
func TestIsSameSessionStepRecord(t *testing.T) {
	if !isSameSessionStepRecord(stepRec("a", "s1"), "s1") {
		t.Error("same-session System step record should match")
	}
	if isSameSessionStepRecord(stepRec("a", "s2"), "s1") {
		t.Error("cross-session step record must NOT match")
	}
	rem := domain.Document{Metadata: map[string]interface{}{"source_agent": "analyst_agent", "session_id": "s1"}}
	if isSameSessionStepRecord(rem, "s1") {
		t.Error("same-session remember() fact (agent-sourced) must NOT match — exclusion is narrow")
	}
	if isSameSessionStepRecord(stepRec("a", "s1"), "") {
		t.Error("empty session id must not filter")
	}
}

// Recall excludes the run's own step records but keeps cross-session step
// records, deliberate remembers, and plain facts.
func TestQuerySearch_ExcludesSameSessionStepRecords(t *testing.T) {
	store := &scopeApplyingStore{docs: []domain.Document{
		stepRec("own", "s1"),   // same-session step record → DROP
		stepRec("other", "s2"), // cross-session step record → KEEP
		{ID: "remembered", Metadata: map[string]interface{}{"source_agent": "analyst", "session_id": "s1"}}, // KEEP
		{ID: "fact", Text: "a plain fact"}, // KEEP
	}}
	qs := NewQueryService(&fakeEmbedder{}, store)
	ctx := domain.WithSessionID(context.Background(), "s1")

	got, err := qs.Search(ctx, "query", "analyst")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	ids := map[string]bool{}
	for _, r := range got {
		ids[r.Document.ID] = true
	}
	if ids["own"] {
		t.Error("same-session step record 'own' must be excluded")
	}
	if !ids["other"] || !ids["remembered"] || !ids["fact"] {
		t.Errorf("cross-session / remembered / plain facts must be kept; got %v", ids)
	}
}

// The returned window is truncated to recallTopK even when the store over-returns.
func TestQuerySearch_TruncatesToTopK(t *testing.T) {
	var docs []domain.Document
	for i := 0; i < 15; i++ {
		docs = append(docs, domain.Document{ID: fmt.Sprintf("f%d", i), Text: "fact"})
	}
	qs := NewQueryService(&fakeEmbedder{}, &scopeApplyingStore{docs: docs})
	got, err := qs.Search(context.Background(), "q", "agent")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != defaultRecallTopK {
		t.Errorf("expected %d results, got %d", defaultRecallTopK, len(got))
	}
}
