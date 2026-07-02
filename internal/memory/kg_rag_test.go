package memory

import (
	"context"
	"strings"
	"testing"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// fakeChunkTripletsStore is an in-memory ChunkTripletsStore for unit tests.
// No DB; no LLM. Mirrors the production semantics: Save is idempotent on
// (chunkID, h, r, t), ForChunks returns the per-chunk list, and
// ChunksMentioningEntity scans the inverted index (head or tail).
type fakeChunkTripletsStore struct {
	byChunkID map[string][]ChunkTriplet
}

func newFakeChunkTripletsStore() *fakeChunkTripletsStore {
	return &fakeChunkTripletsStore{byChunkID: map[string][]ChunkTriplet{}}
}

func (f *fakeChunkTripletsStore) SaveChunkTriplets(_ context.Context, chunkID string, triplets []ChunkTriplet) error {
	if f.byChunkID == nil {
		f.byChunkID = map[string][]ChunkTriplet{}
	}
	existing := f.byChunkID[chunkID]
	keyOf := func(t ChunkTriplet) string { return t.H + "##" + t.R + "##" + t.T }
	seen := make(map[string]bool, len(existing))
	for _, t := range existing {
		seen[keyOf(t)] = true
	}
	for _, t := range triplets {
		k := keyOf(t)
		if seen[k] {
			continue
		}
		seen[k] = true
		existing = append(existing, t)
	}
	f.byChunkID[chunkID] = existing
	return nil
}

func (f *fakeChunkTripletsStore) ForChunk(_ context.Context, chunkID string) ([]ChunkTriplet, error) {
	return f.byChunkID[chunkID], nil
}

func (f *fakeChunkTripletsStore) ForChunks(_ context.Context, chunkIDs []string) (map[string][]ChunkTriplet, error) {
	out := make(map[string][]ChunkTriplet, len(chunkIDs))
	for _, id := range chunkIDs {
		out[id] = f.byChunkID[id]
	}
	return out, nil
}

func (f *fakeChunkTripletsStore) ChunksMentioningEntity(_ context.Context, entity string, limit int) ([]string, error) {
	e := strings.ToLower(strings.TrimSpace(entity))
	if e == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	var out []string
	for cid, triplets := range f.byChunkID {
		for _, t := range triplets {
			if t.H == e || t.T == e {
				out = append(out, cid)
				break
			}
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// fakeVectorSearch satisfies kgExpandVectorSearch for tests — a simple map
// of docID -> Document. GetByID returns the doc or an error if missing.
type fakeVectorSearch struct {
	docs map[string]domain.Document
}

func (f fakeVectorSearch) GetByID(_ context.Context, id string) (*domain.Document, error) {
	d, ok := f.docs[id]
	if !ok {
		return nil, nil
	}
	return &d, nil
}

func TestParseChunkTripletOutput_Basic(t *testing.T) {
	resp := `<caroline##researched##quantum>$$<caroline##lives in##melbourne>`
	got := parseChunkTripletOutput(resp)
	if len(got) != 2 {
		t.Fatalf("expected 2 triplets, got %d: %+v", len(got), got)
	}
	if got[0].H != "caroline" || got[0].R != "researched" || got[0].T != "quantum" {
		t.Errorf("triplet[0] wrong: %+v", got[0])
	}
	if got[1].H != "caroline" || got[1].R != "lives in" || got[1].T != "melbourne" {
		t.Errorf("triplet[1] wrong: %+v", got[1])
	}
}

func TestParseChunkTripletOutput_FiltersNulls(t *testing.T) {
	resp := `<caroline##researched##quantum>$$<caroline##is##no>$$<no##relation##bob>$$<alice##knows##null>`
	got := parseChunkTripletOutput(resp)
	if len(got) != 1 {
		t.Fatalf("expected 1 valid triplet, got %d: %+v", len(got), got)
	}
	if got[0].T != "quantum" {
		t.Errorf("expected quantum as t, got %+v", got[0])
	}
}

func TestParseChunkTripletOutput_FiltersSelfLoops(t *testing.T) {
	resp := `<caroline##knows##caroline>$$<bob##works with##bob>`
	got := parseChunkTripletOutput(resp)
	if len(got) != 0 {
		t.Errorf("self-loops should be filtered, got %+v", got)
	}
}

func TestParseChunkTripletOutput_Dedupes(t *testing.T) {
	resp := `<caroline##researched##quantum>$$<caroline##researched##quantum>$$<caroline##researched##quantum>`
	got := parseChunkTripletOutput(resp)
	if len(got) != 1 {
		t.Errorf("expected 1 unique triplet, got %d: %+v", len(got), got)
	}
}

func TestParseChunkTripletOutput_LowercasesEntities(t *testing.T) {
	resp := `<Caroline##lives in##Melbourne>`
	got := parseChunkTripletOutput(resp)
	if len(got) != 1 {
		t.Fatalf("expected 1 triplet, got %d", len(got))
	}
	if got[0].H != "caroline" || got[0].T != "melbourne" {
		t.Errorf("expected lowercase entities, got h=%q t=%q", got[0].H, got[0].T)
	}
	if got[0].R != "lives in" {
		t.Errorf("relation should preserve case, got %q", got[0].R)
	}
}

func TestParseChunkTripletOutput_Empty(t *testing.T) {
	got := parseChunkTripletOutput("the LLM wrote nothing useful here")
	if len(got) != 0 {
		t.Errorf("expected 0 triplets, got %d: %+v", len(got), got)
	}
}

func TestKgExpand_OneHop_AddsRelatedChunks(t *testing.T) {
	// Seed chunk: "caroline researched quantum"
	// Related chunk (one hop): "quantum was developed at IBM"
	// The expansion should add the related chunk because it shares entity "quantum".
	store := newFakeChunkTripletsStore()
	_ = store.SaveChunkTriplets(context.Background(), "seed-1", []ChunkTriplet{
		{H: "caroline", R: "researched", T: "quantum"},
	})
	_ = store.SaveChunkTriplets(context.Background(), "chunk-2", []ChunkTriplet{
		{H: "quantum", R: "developed at", T: "ibm"},
	})
	_ = store.SaveChunkTriplets(context.Background(), "chunk-3", []ChunkTriplet{
		{H: "alice", R: "knows", T: "bob"}, // unrelated
	})

	seeds := []domain.SearchResult{
		{Document: domain.Document{ID: "seed-1", Text: "caroline researched quantum"}, Score: 0.9},
	}
	vs := fakeVectorSearch{docs: map[string]domain.Document{
		"seed-1":  seeds[0].Document,
		"chunk-2": {ID: "chunk-2", Text: "quantum was developed at IBM"},
		"chunk-3": {ID: "chunk-3", Text: "alice knows bob"},
	}}

	got := kgExpand(context.Background(), seeds, store, vs, nil, kgExpandOpts{Hops: 1, MaxExpanded: 10, MaxEntities: 10})

	if len(got) < 2 {
		t.Fatalf("expected seed + at least 1 expanded, got %d: %+v", len(got), got)
	}
	if got[0].Document.ID != "seed-1" {
		t.Errorf("seed should be first, got %q", got[0].Document.ID)
	}
	hasChunk2 := false
	for _, r := range got {
		if r.Document.ID == "chunk-2" {
			hasChunk2 = true
		}
		if r.Document.ID == "chunk-3" {
			t.Errorf("chunk-3 (no shared entity) should NOT be in expansion")
		}
	}
	if !hasChunk2 {
		t.Errorf("expected chunk-2 in expansion; got %+v", got)
	}
}

func TestKgExpand_NoTriplets_ReturnsSeeds(t *testing.T) {
	store := newFakeChunkTripletsStore() // empty
	seeds := []domain.SearchResult{
		{Document: domain.Document{ID: "seed-1"}, Score: 0.9},
	}
	vs := fakeVectorSearch{docs: map[string]domain.Document{"seed-1": seeds[0].Document}}
	got := kgExpand(context.Background(), seeds, store, vs, nil, kgExpandOpts{})
	if len(got) != 1 || got[0].Document.ID != "seed-1" {
		t.Errorf("expected just the seed, got %+v", got)
	}
}

func TestKgExpand_OneHopLimit(t *testing.T) {
	// seed mentions "quantum"; chunk-2 mentions "quantum"; chunk-3 mentions
	// "ibm" (mentioned in chunk-2). Two-hop should reach chunk-3; one-hop
	// should NOT.
	store := newFakeChunkTripletsStore()
	_ = store.SaveChunkTriplets(context.Background(), "seed-1", []ChunkTriplet{
		{H: "caroline", R: "researched", T: "quantum"},
	})
	_ = store.SaveChunkTriplets(context.Background(), "chunk-2", []ChunkTriplet{
		{H: "quantum", R: "developed at", T: "ibm"},
	})
	_ = store.SaveChunkTriplets(context.Background(), "chunk-3", []ChunkTriplet{
		{H: "ibm", R: "headquartered in", T: "new york"},
	})

	seeds := []domain.SearchResult{
		{Document: domain.Document{ID: "seed-1", Text: "caroline researched quantum"}, Score: 0.9},
	}
	vs := fakeVectorSearch{docs: map[string]domain.Document{
		"seed-1":  seeds[0].Document,
		"chunk-2": {ID: "chunk-2", Text: "quantum was developed at IBM"},
		"chunk-3": {ID: "chunk-3", Text: "IBM is in New York"},
	}}

	// Hops=1: should reach chunk-2 (shared "quantum"), NOT chunk-3.
	got := kgExpand(context.Background(), seeds, store, vs, nil, kgExpandOpts{Hops: 1, MaxExpanded: 20, MaxEntities: 10})
	hasChunk2 := false
	hasChunk3 := false
	for _, r := range got {
		if r.Document.ID == "chunk-2" {
			hasChunk2 = true
		}
		if r.Document.ID == "chunk-3" {
			hasChunk3 = true
		}
	}
	if !hasChunk2 {
		t.Errorf("1-hop should include chunk-2 (shared entity quantum); got %+v", got)
	}
	if hasChunk3 {
		t.Errorf("1-hop should NOT include chunk-3 (would require 2 hops); got %+v", got)
	}
}

func TestKgExpand_RespectsMaxExpanded(t *testing.T) {
	// Seed mentions 5 different entities; each entity has 3 chunks. MaxExpanded=5
	// should cap the result at seed + 5.
	store := newFakeChunkTripletsStore()
	_ = store.SaveChunkTriplets(context.Background(), "seed", []ChunkTriplet{
		{H: "e1", R: "r", T: "e2"},
		{H: "e3", R: "r", T: "e4"},
		{H: "e5", R: "r", T: "e6"},
	})
	// 6 entities x 3 chunks each = 18 candidates
	docs := map[string]domain.Document{"seed": {ID: "seed"}}
	candIdx := 0
	for i := 1; i <= 6; i++ {
		ent := ""
		switch i {
		case 1:
			ent = "e1"
		case 2:
			ent = "e2"
		case 3:
			ent = "e3"
		case 4:
			ent = "e4"
		case 5:
			ent = "e5"
		case 6:
			ent = "e6"
		}
		for j := 0; j < 3; j++ {
			candIdx++
			cid := "cand-" + string(rune('0'+candIdx))
			_ = store.SaveChunkTriplets(context.Background(), cid, []ChunkTriplet{
				{H: ent, R: "r", T: "tail"},
			})
			docs[cid] = domain.Document{ID: cid, Text: ent}
		}
	}

	seeds := []domain.SearchResult{
		{Document: docs["seed"], Score: 0.9},
	}
	got := kgExpand(context.Background(), seeds, store, fakeVectorSearch{docs: docs}, nil,
		kgExpandOpts{Hops: 1, MaxExpanded: 5, MaxEntities: 10})

	if len(got) > 1+5 {
		t.Errorf("expected seed + 5 = 6 max, got %d", len(got))
	}
}
