package memory

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// The spreader fetches every BFS edge target by id and its output REPLACES the
// already-scoped result set in query.go, so it is exactly the by-id enrichment
// path ADR-0085 D7 / ADR-0095 D9 close for the other lanes. Until 2026-10-31 the
// kernel built it over the raw adapter (memory_stack.go), and a cross-scope
// discussed_in/co_activated edge returned the restricted target to any caller
// (T-20261031-spreading-activation-lane-fetches-graph-targets). These tests pin
// the contract at the engine level: built over the enforcing store, the spreader
// never surfaces a target the caller's predicate forbids, and with no predicate
// on the context it surfaces nothing (fail closed).

func scopedSpreadFixture() (*recordingGraphStore, *vecStoreWithDocs) {
	gs := &recordingGraphStore{
		edges: []domain.DocumentEdge{
			{SourceID: "pub-seed", TargetID: "pub-neighbor", EdgeType: domain.EdgeDiscussedIn, Weight: 0.9, CreatedAt: time.Now()},
			{SourceID: "pub-seed", TargetID: "secret", EdgeType: domain.EdgeDiscussedIn, Weight: 0.9, CreatedAt: time.Now()},
		},
	}
	vs := &vecStoreWithDocs{docs: map[string]domain.Document{
		"pub-neighbor": {ID: "pub-neighbor", ActivationStrength: 1.0, Text: "public",
			Metadata: map[string]any{"tags": []string{"public"}}},
		"secret": {ID: "secret", ActivationStrength: 1.0, Text: "internal only",
			Metadata: map[string]any{"tags": []string{"internal_only"}}},
	}}
	return gs, vs
}

func spreadIDs(exp []domain.GraphNodeExpansion) map[string]bool {
	out := make(map[string]bool, len(exp))
	for _, e := range exp {
		out[e.Document.ID] = true
	}
	return out
}

func TestSpreadingEngine_RawStoreLeaksCrossScopeTarget(t *testing.T) {
	// Documents the defect the enforcing wrapper fixes: over the raw store the
	// restricted target comes back. If this ever stops holding, the spreader
	// grew its own filtering — which AGENTS.md §2.5 forbids; the decorator owns it.
	gs, vs := scopedSpreadFixture()
	se := NewSpreadingEngine(gs, vs, 0.75, 3, 0.05)
	ctx := domain.WithScope(context.Background(), &domain.TagPredicate{ForbiddenTags: []string{"internal_only"}})
	got := spreadIDs(se.Spread(ctx, []domain.SearchResult{{Document: domain.Document{ID: "pub-seed", ActivationStrength: 1.0}, Score: 1.0}}))
	if !got["secret"] {
		t.Fatalf("raw store should not filter (that is the decorator's job); got %v", got)
	}
}

func TestSpreadingEngine_EnforcingStoreFiltersCrossScopeTarget(t *testing.T) {
	gs, vs := scopedSpreadFixture()
	se := NewSpreadingEngine(gs, authz.NewEnforcingVectorStore(vs, slog.Default()), 0.75, 3, 0.05)
	ctx := domain.WithScope(context.Background(), &domain.TagPredicate{ForbiddenTags: []string{"internal_only"}})
	got := spreadIDs(se.Spread(ctx, []domain.SearchResult{{Document: domain.Document{ID: "pub-seed", ActivationStrength: 1.0}, Score: 1.0}}))
	if got["secret"] {
		t.Fatalf("restricted target leaked through the spreader: %v", got)
	}
	if !got["pub-neighbor"] {
		t.Fatalf("allowed target should still spread: %v", got)
	}
}

func TestSpreadingEngine_EnforcingStoreFailsClosedWithoutScope(t *testing.T) {
	gs, vs := scopedSpreadFixture()
	se := NewSpreadingEngine(gs, authz.NewEnforcingVectorStore(vs, slog.Default()), 0.75, 3, 0.05)
	got := spreadIDs(se.Spread(context.Background(), []domain.SearchResult{{Document: domain.Document{ID: "pub-seed", ActivationStrength: 1.0}, Score: 1.0}}))
	if got["secret"] || got["pub-neighbor"] {
		t.Fatalf("no predicate on ctx must surface no edge targets (fail closed); got %v", got)
	}
}
