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

// Recall excludes the run's own step records AND every other session's material,
// while keeping deliberate remembers and unowned corpus facts.
//
// The cross-session expectation here was INVERTED by BRAIN-01, deliberately. This
// test used to assert that a step record from session s2 was KEPT while answering
// as s1 — which is exactly the bleed BRAIN-01 exists to remove. ADR-0048 D1's
// narrow exclusion was never a claim that cross-session material *should* surface;
// isolation simply did not exist yet, and the kernel's only session-aware filter
// dropped your own records and kept everyone else's.
//
// What is unchanged: a same-session remember() is agent-sourced and still kept
// (the D1 exclusion stays narrow), and an unowned corpus fact carries no session
// id and stays visible to everyone — isolation is a predicate, not a store reset.
func TestQuerySearch_ExcludesSameSessionStepRecords(t *testing.T) {
	store := &scopeApplyingStore{docs: []domain.Document{
		stepRec("own", "s1"),   // same-session step record → DROP
		stepRec("other", "s2"), // ANOTHER session's step record → DROP (BRAIN-01)
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
	if ids["other"] {
		t.Error("another session's record leaked into this conversation (BRAIN-01)")
	}
	if !ids["remembered"] || !ids["fact"] {
		t.Errorf("same-session remembers and unowned corpus facts must be kept; got %v", ids)
	}
}

// Isolation must not empty the corpus. A deployment whose ingested knowledge
// carries no session id keeps every bit of it — this is the difference between a
// predicate and a store reset, and getting it wrong would delete the knowledge
// base the day isolation shipped.
func TestQuerySearch_IsolationKeepsUnownedCorpus(t *testing.T) {
	store := &scopeApplyingStore{docs: []domain.Document{
		{ID: "corpus-1", Text: "a plain fact"},
		{ID: "corpus-2", Text: "another plain fact"},
		stepRec("theirs", "s2"),
	}}
	qs := NewQueryService(&fakeEmbedder{}, store)
	got, err := qs.Search(domain.WithSessionID(context.Background(), "s1"), "query", "analyst")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	ids := map[string]bool{}
	for _, r := range got {
		ids[r.Document.ID] = true
	}
	if !ids["corpus-1"] || !ids["corpus-2"] {
		t.Errorf("isolation removed unowned corpus material; got %v", ids)
	}
	if ids["theirs"] {
		t.Errorf("another session's material survived; got %v", ids)
	}
}

// With NO session on the context there is no conversation to isolate to — a
// kernel or operator read. That is a bypass, not a denial: denying would make
// every unattended read return nothing, a far larger blast radius than the bleed
// being fixed.
func TestQuerySearch_NoSessionMeansNoNarrowing(t *testing.T) {
	store := &scopeApplyingStore{docs: []domain.Document{
		stepRec("a", "s1"),
		stepRec("b", "s2"),
		{ID: "corpus", Text: "a plain fact"},
	}}
	qs := NewQueryService(&fakeEmbedder{}, store)
	got, err := qs.Search(context.Background(), "query", "analyst")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("an unattended read was narrowed: got %d of 3", len(got))
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
