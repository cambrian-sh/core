package memory

import (
	"context"
	"reflect"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// ENG-484: spreadAndRank used to re-sort the whole assembled window by activation
// energy, so an injected row (assembled AFTER the primary hits, non-displacing) that
// out-scored a primary hit moved ahead of it — and into the caller's top_k when the
// caller trims the window. With recall_spreading_keep_assembly_order the seeds keep
// their assembled order and only rows spreading reached are placed, by energy, after.

func TestSpreadAndRank_KeepAssemblyOrder(t *testing.T) {
	t.Parallel()
	seeds := []domain.SearchResult{
		{Document: domain.Document{ID: "primary"}, Score: 0.2},  // a primary hit, assembled first
		{Document: domain.Document{ID: "injected"}, Score: 0.5}, // a 0.5-floor injected row
	}
	spr := &fakeSpreader{extra: []domain.GraphNodeExpansion{
		{Document: domain.Document{ID: "reached-hi"}, ActivationEnergy: 0.9},
		{Document: domain.Document{ID: "reached-lo"}, ActivationEnergy: 0.1},
	}}
	run := func(on bool) []string {
		q := NewQueryService(&fakeEmbedder{}, &scopeApplyingStore{}, nil)
		q.EnableSpreading(spr)
		q.SetSpreadingKeepsAssemblyOrder(on)
		return ids(q.spreadAndRank(context.Background(), seeds, ""))
	}
	if got, want := run(false), []string{"reached-hi", "injected", "primary", "reached-lo"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("off (shipped): energy order %v, want %v", got, want)
	}
	if got, want := run(true), []string{"primary", "injected", "reached-hi", "reached-lo"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("keep order: %v, want seeds in assembled order then reached rows by energy %v", got, want)
	}
}

// End to end through QueryService.Search: the recall a caller gets keeps the
// assembled seeds first and places the graph-linked row after them.
func TestQuerySearch_SpreadingKeepsAssemblyOrder(t *testing.T) {
	t.Parallel()
	qs := NewQueryService(&fakeEmbedder{}, &scopeApplyingStore{docs: []domain.Document{
		{ID: "seed", Text: "seed fact"},
	}}, nil)
	qs.EnableSpreading(&fakeSpreader{extra: []domain.GraphNodeExpansion{
		{Document: domain.Document{ID: "linked", Text: "graph-linked fact"}, ActivationEnergy: 0.9},
	}})
	qs.SetSpreadingKeepsAssemblyOrder(true)
	got, err := qs.Search(context.Background(), "q", "agent")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if want := []string{"seed", "linked"}; !reflect.DeepEqual(ids(got), want) {
		t.Fatalf("Search = %v, want %v (TestQuerySearch_SpreadingExpandsAndRanks is the flag-off order)", ids(got), want)
	}
}
