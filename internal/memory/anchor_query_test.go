package memory

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

func hasAnchor(anchors []string, want string) bool {
	for _, a := range anchors {
		if a == want {
			return true
		}
	}
	return false
}

func TestExtractQueryAnchors_ChapterScene(t *testing.T) {
	got := extractQueryAnchors("What coded phrase appears in Chapter 1, scene 1?")
	for _, want := range []string{"chapter_scene:1/1", "chapter:1", "scene:1"} {
		if !hasAnchor(got, want) {
			t.Fatalf("expected %q in anchors, got %v", want, got)
		}
	}
	// The compound (specific) must sort before the broad atomics.
	if got[0] != "chapter_scene:1/1" {
		t.Fatalf("compound anchor must lead; got %v", got)
	}
}

func TestExtractQueryAnchors_RomanAndDecimalAndId(t *testing.T) {
	if a := extractQueryAnchors("In Chapter IV, scene 3, what happened?"); !hasAnchor(a, "chapter_scene:4/3") {
		t.Fatalf("roman chapter compound missing: %v", a)
	}
	if a := extractQueryAnchors("What does Section 3.2 specify?"); !hasAnchor(a, "section:3.2") {
		t.Fatalf("decimal section missing: %v", a)
	}
	// Multi-hop citing raw chunk ids → raw tokens present (match the anchor SUBJECT).
	a := extractQueryAnchors("Add the ledger amounts from tidebound-archive-chunk-5 and tidebound-archive-chunk-15.")
	if !hasAnchor(a, "tidebound-archive-chunk-5") || !hasAnchor(a, "tidebound-archive-chunk-15") {
		t.Fatalf("cited chunk ids missing: %v", a)
	}
}

func TestExtractQueryAnchors_NoAnchorFallback(t *testing.T) {
	if a := extractQueryAnchors("Who kept watch with the amber compass?"); len(a) != 0 {
		t.Fatalf("a query with no reference system must yield no anchors, got %v", a)
	}
}

// The gold chunk is buried at the BOTTOM of the reranked pool (all chunks look
// alike to the embedder). The query names its anchor; promotion must lift it to
// the front, above the floor.
func TestApplyAnchorConstraint_PromotesBuriedChunk(t *testing.T) {
	st := newFakeChunkTripletsStore()
	// gold carries the compound anchor for Chapter 1, scene 1 (subject = doc id).
	_ = st.SaveChunkTriplets(context.Background(), "gold-1", []ChunkTriplet{
		{H: "gold-1", R: "has_anchor", T: "chapter:1"},
		{H: "gold-1", R: "has_anchor", T: "scene:1"},
		{H: "gold-1", R: "has_anchor", T: "chapter_scene:1/1"},
	})
	vs := &fakeVecStore{docs: map[string]domain.Document{
		"gold-1": {ID: "gold-1"}, "n1": {ID: "n1"}, "n2": {ID: "n2"},
	}}
	q := &QueryService{chunkTriplets: st, vectorStore: vs, anchorConstraint: true, kgPerEntity: 5, floor: 0.3}

	// Reranked pool: noise on top, gold buried last with a sub-floor score.
	pool := []domain.SearchResult{
		{Document: domain.Document{ID: "n1"}, Score: 0.8},
		{Document: domain.Document{ID: "n2"}, Score: 0.7},
		{Document: domain.Document{ID: "gold-1"}, Score: 0.05},
	}
	out := q.applyAnchorConstraint(context.Background(), pool, "What coded phrase appears in Chapter 1, scene 1?", nil)

	if out[0].Document.ID != "gold-1" {
		t.Fatalf("anchored gold chunk must lead; got order %v", ids(out))
	}
	if out[0].Score < 1.0 {
		t.Fatalf("promoted chunk must clear the floor with score >= 1.0; got %v", out[0].Score)
	}
}

// A specific compound anchor must NOT drag in the ten chunks that share only the
// broad chapter:1 atomic.
func TestApplyAnchorConstraint_PrefersSpecific(t *testing.T) {
	st := newFakeChunkTripletsStore()
	_ = st.SaveChunkTriplets(context.Background(), "gold", []ChunkTriplet{
		{H: "gold", R: "has_anchor", T: "chapter:1"},
		{H: "gold", R: "has_anchor", T: "chapter_scene:1/1"},
	})
	// A sibling in the same chapter but a different scene: shares chapter:1 only.
	_ = st.SaveChunkTriplets(context.Background(), "sibling", []ChunkTriplet{
		{H: "sibling", R: "has_anchor", T: "chapter:1"},
		{H: "sibling", R: "has_anchor", T: "chapter_scene:1/2"},
	})
	vs := &fakeVecStore{docs: map[string]domain.Document{"gold": {ID: "gold"}, "sibling": {ID: "sibling"}}}
	q := &QueryService{chunkTriplets: st, vectorStore: vs, anchorConstraint: true, kgPerEntity: 5, floor: 0.3}

	out := q.applyAnchorConstraint(context.Background(), nil, "In Chapter 1, scene 1, which artifact?", nil)
	if len(out) != 1 || out[0].Document.ID != "gold" {
		t.Fatalf("only the compound-matched chunk should be promoted; got %v", ids(out))
	}
}

// Two cited chunk ids (multi-hop) must both be promoted so the pair lands in the
// returned window.
func TestApplyAnchorConstraint_MultiHopTwoIds(t *testing.T) {
	st := newFakeChunkTripletsStore()
	id5, id15 := "tidebound-archive-chunk-5", "tidebound-archive-chunk-15"
	_ = st.SaveChunkTriplets(context.Background(), id5, []ChunkTriplet{{H: id5, R: "has_anchor", T: "chapter:1"}})
	_ = st.SaveChunkTriplets(context.Background(), id15, []ChunkTriplet{{H: id15, R: "has_anchor", T: "chapter:2"}})
	vs := &fakeVecStore{docs: map[string]domain.Document{id5: {ID: id5}, id15: {ID: id15}}}
	q := &QueryService{chunkTriplets: st, vectorStore: vs, anchorConstraint: true, kgPerEntity: 5, floor: 0.3}

	out := q.applyAnchorConstraint(context.Background(), nil,
		"Add the ledger amounts from "+id5+" and "+id15+". What is the combined total?", nil)
	var got5, got15 bool
	for _, r := range out {
		if r.Document.ID == id5 {
			got5 = true
		}
		if r.Document.ID == id15 {
			got15 = true
		}
	}
	if !got5 || !got15 {
		t.Fatalf("both cited chunk ids must be promoted; got %v", ids(out))
	}
}

func TestApplyAnchorConstraint_NoAnchorIsNoop(t *testing.T) {
	st := newFakeChunkTripletsStore()
	_ = st.SaveChunkTriplets(context.Background(), "x", []ChunkTriplet{{H: "x", R: "has_anchor", T: "chapter:1"}})
	vs := &fakeVecStore{docs: map[string]domain.Document{"x": {ID: "x"}}}
	q := &QueryService{chunkTriplets: st, vectorStore: vs, anchorConstraint: true, kgPerEntity: 5, floor: 0.3}

	pool := []domain.SearchResult{{Document: domain.Document{ID: "x"}, Score: 0.9}}
	out := q.applyAnchorConstraint(context.Background(), pool, "Who is the harbor magistrate?", nil)
	if len(out) != 1 || out[0].Document.ID != "x" || out[0].Score != 0.9 {
		t.Fatalf("anchorless query must leave the pool untouched; got %v", ids(out))
	}
}

func ids(rs []domain.SearchResult) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Document.ID
	}
	return out
}
