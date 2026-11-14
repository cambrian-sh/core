package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/authz"
)

// The enrichment lanes (KG expansion, query-entity seeding) no longer re-apply the
// TagPredicate inline: they stamp it on ctx and the
// tier-1 authz.EnforcingVectorStore applies it (AGENTS.md §2.5,
// T-20261113-retrieval-enrichment-lanes-re-apply-the). These tests build the lanes
// over the REAL decorator — a raw fake would not filter at all, so a test over one
// could not prove a forbidden row is still dropped.

// mapInner is the raw (tier-0) store under the decorator: by-id reads only, no
// filtering of its own.
type mapInner struct {
	domain.VectorStore
	docs map[string]domain.Document
}

func (m mapInner) GetByID(_ context.Context, id string) (*domain.Document, error) {
	d, ok := m.docs[id]
	if !ok {
		return nil, nil
	}
	return &d, nil
}

func (m mapInner) GetBatch(_ context.Context, ids []string) ([]domain.Document, error) {
	out := make([]domain.Document, 0, len(ids))
	for _, id := range ids {
		if d, ok := m.docs[id]; ok {
			out = append(out, d)
		}
	}
	return out, nil
}

func enforcedStore() *authz.EnforcingVectorStore {
	return authz.NewEnforcingVectorStore(mapInner{docs: map[string]domain.Document{
		"public": {ID: "public", Text: "public row"},
		"secret": {ID: "secret", Text: "sk-live secret row",
			Metadata: map[string]interface{}{"tags": []string{"internal"}}},
	}}, nil)
}

var forbidInternal = &domain.TagPredicate{ForbiddenTags: []string{"internal"}}

// Batched path (ChunksByIDs on the decorator): the forbidden row is absent, the
// permitted one admitted — the parameter predicate is the one the decorator applied.
func TestMaterializeAuthorized_DecoratorAppliesTheParameterPredicate(t *testing.T) {
	t.Parallel()
	// ctx carries NO scope: without the stamp the decorator would refuse the read
	// (ErrScopeMissing) and the batch would fall back and admit nothing.
	got := materializeAuthorized(context.Background(), enforcedStore(), forbidInternal, []string{"public", "secret"})
	if _, ok := got["secret"]; ok {
		t.Fatal("forbidden row admitted on the batched path")
	}
	if _, ok := got["public"]; !ok {
		t.Fatalf("permitted row dropped on the batched path: %+v", got)
	}
}

// Per-id path: same answer as the batched one.
func TestAuthorizedDoc_DecoratorAppliesTheParameterPredicate(t *testing.T) {
	t.Parallel()
	st := enforcedStore()
	if _, ok := authorizedDoc(context.Background(), st, forbidInternal, "secret"); ok {
		t.Fatal("forbidden row admitted on the per-id path")
	}
	if _, ok := authorizedDoc(context.Background(), st, forbidInternal, "public"); !ok {
		t.Fatal("permitted row dropped on the per-id path")
	}
	if _, ok := authorizedDoc(context.Background(), st, nil, "public"); ok {
		t.Fatal("a nil predicate admitted a row")
	}
}

// The parameter wins over whatever the caller's ctx happened to carry: a wider
// (bypass) scope on ctx must not widen the lane's predicate.
func TestMaterializeAuthorized_ParameterOverridesCtxScope(t *testing.T) {
	t.Parallel()
	ctx := domain.WithScope(context.Background(), &domain.TagPredicate{Bypass: true})
	got := materializeAuthorized(ctx, enforcedStore(), forbidInternal, []string{"public", "secret"})
	if _, ok := got["secret"]; ok {
		t.Fatal("a bypass scope on ctx widened the lane's predicate")
	}
}

// Legacy per-term query-entity seeding (store without the batched entity lookup)
// reads through the decorator under the lane's predicate.
func TestInjectQueryEntitySeeds_LegacyPathDropsForbiddenThroughDecorator(t *testing.T) {
	t.Parallel()
	st := newFakeChunkTripletsStore()
	_ = st.SaveChunkTriplets(context.Background(), "public", []domain.ChunkTriplet{{H: "caroline", R: "moved from", T: "seattle"}})
	_ = st.SaveChunkTriplets(context.Background(), "secret", []domain.ChunkTriplet{{H: "caroline", R: "billed via", T: "stripe"}})
	q := &QueryService{chunkTriplets: st, vectorStore: enforcedStore(), queryEntitySeed: true, kgPerEntity: 5, kgMaxExpanded: 20}

	out := q.injectQueryEntitySeeds(context.Background(), nil, "Where did Caroline move from?", nil, forbidInternal)
	var sawPublic bool
	for _, r := range out {
		if r.Document.ID == "secret" {
			t.Fatal("forbidden chunk seeded")
		}
		sawPublic = sawPublic || r.Document.ID == "public"
	}
	if !sawPublic {
		t.Fatalf("permitted chunk not seeded: %+v", out)
	}
}

// ENG-540 regression (owner decision 2026-11-15): MemoryManager's by-id reads apply
// the CALLER's predicate through the real decorator instead of overwriting it with
// ScopeSystem; with no predicate they fail closed.
func TestMemoryManager_ByIDReadsKeepTheCallersScope(t *testing.T) {
	t.Parallel()
	m := NewMemoryManager(enforcedStore(), &fakeEmbedder{})
	ctx := domain.WithScope(context.Background(), forbidInternal)

	if d, err := m.GetByID(ctx, "secret"); err != nil || d != nil {
		t.Fatalf("a row the caller may not read must come back absent, got %+v, %v", d, err)
	}
	if d, err := m.GetByID(ctx, "public"); err != nil || d == nil {
		t.Fatalf("a permitted row must be read, got %+v, %v", d, err)
	}
	docs, err := m.GetBatch(ctx, []string{"public", "secret"})
	if err != nil || len(docs) != 1 || docs[0].ID != "public" {
		t.Fatalf("GetBatch under the caller's scope = %+v, %v", docs, err)
	}
	if _, err := m.GetByID(context.Background(), "public"); !errors.Is(err, authz.ErrScopeMissing) {
		t.Fatalf("no predicate must fail closed, got %v", err)
	}
}
