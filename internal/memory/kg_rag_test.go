package memory

import (
	"context"
	"strings"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// fakeChunkTripletsStore is an in-memory domain.ChunkTripletsStore for unit tests.
// No DB; no LLM. Mirrors the production semantics: Save is idempotent on
// (chunkID, h, r, t), ForChunks returns the per-chunk list, and
// ChunksMentioningEntity scans the inverted index (head or tail).
type fakeChunkTripletsStore struct {
	byChunkID map[string][]domain.ChunkTriplet
	// lastScope records the predicate the caller threaded through, so a test can
	// assert the ADR-0095 D9 wiring exists rather than trusting it.
	lastScope *domain.TagPredicate
	scopeSeen bool
}

func newFakeChunkTripletsStore() *fakeChunkTripletsStore {
	return &fakeChunkTripletsStore{byChunkID: map[string][]domain.ChunkTriplet{}}
}

func (f *fakeChunkTripletsStore) SaveChunkTriplets(_ context.Context, chunkID string, triplets []domain.ChunkTriplet) error {
	if f.byChunkID == nil {
		f.byChunkID = map[string][]domain.ChunkTriplet{}
	}
	existing := f.byChunkID[chunkID]
	keyOf := func(t domain.ChunkTriplet) string { return t.H + "##" + t.R + "##" + t.T }
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

func (f *fakeChunkTripletsStore) ForChunk(_ context.Context, chunkID string) ([]domain.ChunkTriplet, error) {
	return f.byChunkID[chunkID], nil
}

func (f *fakeChunkTripletsStore) ForChunks(_ context.Context, chunkIDs []string) (map[string][]domain.ChunkTriplet, error) {
	out := make(map[string][]domain.ChunkTriplet, len(chunkIDs))
	for _, id := range chunkIDs {
		out[id] = f.byChunkID[id]
	}
	return out, nil
}

func (f *fakeChunkTripletsStore) ChunksMentioningEntity(_ context.Context, entity string, limit int, scope *domain.TagPredicate) ([]string, error) {
	f.lastScope = scope
	f.scopeSeen = true
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
	_ = store.SaveChunkTriplets(context.Background(), "seed-1", []domain.ChunkTriplet{
		{H: "caroline", R: "researched", T: "quantum"},
	})
	_ = store.SaveChunkTriplets(context.Background(), "chunk-2", []domain.ChunkTriplet{
		{H: "quantum", R: "developed at", T: "ibm"},
	})
	_ = store.SaveChunkTriplets(context.Background(), "chunk-3", []domain.ChunkTriplet{
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

	got := kgExpand(context.Background(), seeds, store, vs, nil, &domain.TagPredicate{Bypass: true}, kgExpandOpts{Hops: 1, MaxExpanded: 10, MaxEntities: 10})

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
	got := kgExpand(context.Background(), seeds, store, vs, nil, &domain.TagPredicate{Bypass: true}, kgExpandOpts{})
	if len(got) != 1 || got[0].Document.ID != "seed-1" {
		t.Errorf("expected just the seed, got %+v", got)
	}
}

func TestKgExpand_OneHopLimit(t *testing.T) {
	// seed mentions "quantum"; chunk-2 mentions "quantum"; chunk-3 mentions
	// "ibm" (mentioned in chunk-2). Two-hop should reach chunk-3; one-hop
	// should NOT.
	store := newFakeChunkTripletsStore()
	_ = store.SaveChunkTriplets(context.Background(), "seed-1", []domain.ChunkTriplet{
		{H: "caroline", R: "researched", T: "quantum"},
	})
	_ = store.SaveChunkTriplets(context.Background(), "chunk-2", []domain.ChunkTriplet{
		{H: "quantum", R: "developed at", T: "ibm"},
	})
	_ = store.SaveChunkTriplets(context.Background(), "chunk-3", []domain.ChunkTriplet{
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
	got := kgExpand(context.Background(), seeds, store, vs, nil, &domain.TagPredicate{Bypass: true}, kgExpandOpts{Hops: 1, MaxExpanded: 20, MaxEntities: 10})
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
	_ = store.SaveChunkTriplets(context.Background(), "seed", []domain.ChunkTriplet{
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
			_ = store.SaveChunkTriplets(context.Background(), cid, []domain.ChunkTriplet{
				{H: ent, R: "r", T: "tail"},
			})
			docs[cid] = domain.Document{ID: cid, Text: ent}
		}
	}

	seeds := []domain.SearchResult{
		{Document: docs["seed"], Score: 0.9},
	}
	got := kgExpand(context.Background(), seeds, store, fakeVectorSearch{docs: docs}, nil, &domain.TagPredicate{Bypass: true},
		kgExpandOpts{Hops: 1, MaxExpanded: 5, MaxEntities: 10})

	if len(got) > 1+5 {
		t.Errorf("expected seed + 5 = 6 max, got %d", len(got))
	}
}

// TestKGExpand_DropsForbiddenChunk is the ADR-0095 D9 regression.
//
// KG expansion reaches chunks BY ID (ChunksMentioningEntity -> GetByID), and neither
// hop is enforced: chunk_triplets has no classification column, and EnforcingVectorStore
// overrides Search only, so GetByID reads the raw adapter. The predicate must therefore be
// applied to what comes back, or a restricted chunk is admitted with its FULL CONTENT.
//
// The prior mustGetDoc also returned domain.Document{ID: id} whenever GetByID failed, so
// even a denied chunk entered the pool as a stub carrying its ID — and `{docID}-chunk-{n}`
// ids encode their source document. Both are asserted here: no content, and no bare id.
func TestKGExpand_DropsForbiddenChunk(t *testing.T) {
	store := newFakeChunkTripletsStore()
	_ = store.SaveChunkTriplets(context.Background(), "seed-1", []domain.ChunkTriplet{
		{H: "caroline", R: "researched", T: "quantum"},
	})
	// Both mention `quantum`, so both are reachable from the seed by one hop.
	_ = store.SaveChunkTriplets(context.Background(), "chunk-public", []domain.ChunkTriplet{
		{H: "quantum", R: "developed at", T: "ibm"},
	})
	_ = store.SaveChunkTriplets(context.Background(), "chunk-secret", []domain.ChunkTriplet{
		{H: "quantum", R: "billed via", T: "stripe"},
	})

	const secret = "internal billing api key sk-live-9f3c endpoint https://billing.internal"
	seeds := []domain.SearchResult{
		{Document: domain.Document{ID: "seed-1", Text: "caroline researched quantum"}, Score: 0.9},
	}
	vs := fakeVectorSearch{docs: map[string]domain.Document{
		"seed-1":       seeds[0].Document,
		"chunk-public": {ID: "chunk-public", Text: "quantum was developed at IBM"},
		"chunk-secret": {
			ID:       "chunk-secret",
			Text:     secret,
			Metadata: map[string]interface{}{"tags": []string{"internal"}},
		},
	}}

	// A customer-surface predicate: `internal` is forbidden, everything else allowed.
	scope := &domain.TagPredicate{ForbiddenTags: []string{"internal"}}
	got := kgExpand(context.Background(), seeds, store, vs, nil, scope,
		kgExpandOpts{Hops: 1, MaxExpanded: 10, MaxEntities: 10})

	for _, r := range got {
		if r.Document.ID == "chunk-secret" {
			t.Errorf("forbidden chunk admitted (id leaked): %+v", r.Document)
		}
		if strings.Contains(r.Document.Text, "sk-live-9f3c") {
			t.Errorf("forbidden chunk CONTENT admitted: %q", r.Document.Text)
		}
	}

	// Not over-blocking: the permitted neighbour still expands.
	var sawPublic bool
	for _, r := range got {
		if r.Document.ID == "chunk-public" {
			sawPublic = true
		}
	}
	if !sawPublic {
		t.Errorf("permitted chunk was dropped; expansion over-blocked: %+v", got)
	}
}

// TestKGExpand_NilPredicateFailsClosed: a nil predicate means no read is authorized
// (readFilter's contract), so expansion must admit nothing rather than everything.
// This is what makes the required `scope` parameter safe to forget.
func TestKGExpand_NilPredicateFailsClosed(t *testing.T) {
	store := newFakeChunkTripletsStore()
	_ = store.SaveChunkTriplets(context.Background(), "seed-1", []domain.ChunkTriplet{
		{H: "caroline", R: "researched", T: "quantum"},
	})
	_ = store.SaveChunkTriplets(context.Background(), "chunk-2", []domain.ChunkTriplet{
		{H: "quantum", R: "developed at", T: "ibm"},
	})

	seeds := []domain.SearchResult{
		{Document: domain.Document{ID: "seed-1", Text: "caroline researched quantum"}, Score: 0.9},
	}
	vs := fakeVectorSearch{docs: map[string]domain.Document{
		"seed-1":  seeds[0].Document,
		"chunk-2": {ID: "chunk-2", Text: "quantum was developed at IBM"},
	}}

	got := kgExpand(context.Background(), seeds, store, vs, nil, nil,
		kgExpandOpts{Hops: 1, MaxExpanded: 10, MaxEntities: 10})

	// Seeds pass through untouched (they were already authorized upstream); only the
	// expansion is gated.
	if len(got) != 1 || got[0].Document.ID != "seed-1" {
		t.Errorf("nil predicate must admit no expanded chunks, got %d: %+v", len(got), got)
	}
}

// TestKGExpand_ThreadsScopeToTripletStore asserts the ADR-0095 D9 wiring: the caller's
// predicate must actually REACH ChunksMentioningEntity, which applies it by joining the
// chunk rows (chunk_triplets carries no classification of its own).
//
// This is a wiring test on purpose. The predicate check inside authorizedDoc is covered
// by TestKGExpand_DropsForbiddenChunk; what that cannot catch is a lookup that quietly
// stops being passed a scope — the failure mode is invisible in review, because the call
// still compiles and still returns rows.
func TestKGExpand_ThreadsScopeToTripletStore(t *testing.T) {
	store := newFakeChunkTripletsStore()
	_ = store.SaveChunkTriplets(context.Background(), "seed-1", []domain.ChunkTriplet{
		{H: "caroline", R: "researched", T: "quantum"},
	})
	_ = store.SaveChunkTriplets(context.Background(), "chunk-2", []domain.ChunkTriplet{
		{H: "quantum", R: "developed at", T: "ibm"},
	})

	seeds := []domain.SearchResult{
		{Document: domain.Document{ID: "seed-1", Text: "caroline researched quantum"}, Score: 0.9},
	}
	vs := fakeVectorSearch{docs: map[string]domain.Document{
		"seed-1":  seeds[0].Document,
		"chunk-2": {ID: "chunk-2", Text: "quantum was developed at IBM"},
	}}

	scope := &domain.TagPredicate{ForbiddenTags: []string{"internal"}}
	_ = kgExpand(context.Background(), seeds, store, vs, nil, scope,
		kgExpandOpts{Hops: 1, MaxExpanded: 10, MaxEntities: 10})

	if !store.scopeSeen {
		t.Fatal("ChunksMentioningEntity was never called; test cannot prove threading")
	}
	if store.lastScope == nil {
		t.Fatal("scope did not reach ChunksMentioningEntity (nil) — the lookup would be unscoped")
	}
	if len(store.lastScope.ForbiddenTags) != 1 || store.lastScope.ForbiddenTags[0] != "internal" {
		t.Errorf("wrong predicate reached the store: %+v", store.lastScope)
	}
}
