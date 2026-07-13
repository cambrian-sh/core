package memory

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// fakeSpreader echoes the seeds and appends fixed graph-discovered expansions.
type fakeSpreader struct{ extra []domain.GraphNodeExpansion }

func (f *fakeSpreader) Spread(_ context.Context, seeds []domain.SearchResult) []domain.GraphNodeExpansion {
	out := make([]domain.GraphNodeExpansion, 0, len(seeds)+len(f.extra))
	for _, s := range seeds {
		out = append(out, domain.GraphNodeExpansion{Document: s.Document, ActivationEnergy: s.Score})
	}
	return append(out, f.extra...)
}

// With spreading enabled, recall surfaces a graph-linked node a flat top-k would
// have missed, ranked by activation.
func TestQuerySearch_SpreadingExpandsAndRanks(t *testing.T) {
	qs := NewQueryService(&fakeEmbedder{}, &scopeApplyingStore{docs: []domain.Document{
		{ID: "seed", Text: "seed fact"},
	}})
	qs.EnableSpreading(&fakeSpreader{extra: []domain.GraphNodeExpansion{
		{Document: domain.Document{ID: "linked", Text: "graph-linked fact"}, ActivationEnergy: 0.9},
	}})

	got, err := qs.Search(context.Background(), "q", "agent")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	ids := map[string]bool{}
	for _, r := range got {
		ids[r.Document.ID] = true
	}
	if !ids["linked"] || !ids["seed"] {
		t.Errorf("expected both seed and graph-linked node, got %v", ids)
	}
	if got[0].Document.ID != "linked" {
		t.Errorf("higher-activation linked node should rank first, got %s", got[0].Document.ID)
	}
}

// Without EnableSpreading, recall is a flat top-k (no expansion).
func TestQuerySearch_SpreadingDisabledIsFlatTopK(t *testing.T) {
	qs := NewQueryService(&fakeEmbedder{}, &scopeApplyingStore{docs: []domain.Document{{ID: "seed"}}})
	got, err := qs.Search(context.Background(), "q", "agent")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].Document.ID != "seed" {
		t.Errorf("expected flat top-k (seed only), got %+v", got)
	}
}

// Spreading still excludes the current session's own step records (D1 ∩ D2).
func TestQuerySearch_SpreadingExcludesSameSessionStepRecords(t *testing.T) {
	qs := NewQueryService(&fakeEmbedder{}, &scopeApplyingStore{docs: []domain.Document{{ID: "seed"}}})
	qs.EnableSpreading(&fakeSpreader{extra: []domain.GraphNodeExpansion{
		{Document: stepRec("own", "s1"), ActivationEnergy: 0.99}, // same-session step record
	}})
	ctx := domain.WithSessionID(context.Background(), "s1")

	got, _ := qs.Search(ctx, "q", "agent")
	for _, r := range got {
		if r.Document.ID == "own" {
			t.Error("spreading must not re-surface the run's own step record")
		}
	}
}
