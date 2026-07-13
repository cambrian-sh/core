package memory

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

func TestEntityIndex_AddAndDocsFor(t *testing.T) {
	idx := NewEntityIndex()
	idx.Add("named:caroline", "doc1", 0.9, MetaKindNamed, 100)
	idx.Add("named:caroline", "doc2", 0.7, MetaKindNamed, 200)
	idx.Add("concept:adoption", "doc1", 0.6, MetaKindConcept, 150)

	if docs := idx.DocsFor("named:caroline"); len(docs) != 2 {
		t.Fatalf("want 2 docs, got %d", len(docs))
	} else {
		// DocsFor returns weight-desc.
		if docs[0].DocID != "doc1" || docs[0].Weight != 0.9 {
			t.Errorf("expected doc1 first, got %+v", docs[0])
		}
	}
	if docs := idx.DocsFor("concept:adoption"); len(docs) != 1 {
		t.Errorf("want 1 doc, got %d", len(docs))
	}
	if docs := idx.DocsFor("named:nobody"); docs != nil {
		t.Errorf("unknown key should return nil, got %+v", docs)
	}
}

func TestEntityIndex_UpdateUpsertsWeight(t *testing.T) {
	idx := NewEntityIndex()
	idx.Add("named:eve", "d", 0.5, MetaKindNamed, 100)
	idx.Add("named:eve", "d", 0.8, MetaKindNamed, 200) // higher
	docs := idx.DocsFor("named:eve")
	if len(docs) != 1 {
		t.Fatalf("upsert should not duplicate, got %d docs", len(docs))
	}
	if docs[0].Weight != 0.8 {
		t.Errorf("higher weight should win, got %f", docs[0].Weight)
	}
}

func TestEntityIndex_UpdateDoesNotLowerWeight(t *testing.T) {
	idx := NewEntityIndex()
	idx.Add("named:eve", "d", 0.9, MetaKindNamed, 100)
	idx.Add("named:eve", "d", 0.5, MetaKindNamed, 200) // lower
	docs := idx.DocsFor("named:eve")
	if docs[0].Weight != 0.9 {
		t.Errorf("lower weight should not overwrite, got %f", docs[0].Weight)
	}
}

func TestEntityIndex_LookupTopByEmbedding(t *testing.T) {
	idx := NewEntityIndex()
	idx.SetNameEmbedding("named:caroline", "Caroline", domain.Embedding{Vector: []float32{1, 0, 0}})
	idx.SetNameEmbedding("named:sam", "Sam", domain.Embedding{Vector: []float32{0, 1, 0}})
	idx.SetNameEmbedding("concept:adoption", "adoption", domain.Embedding{Vector: []float32{0.7, 0.7, 0}})

	got := idx.LookupTopByEmbedding(domain.Embedding{Vector: []float32{1, 0.1, 0}}, 2)
	if len(got) != 2 {
		t.Fatalf("want 2 results, got %d", len(got))
	}
	if got[0] != "named:caroline" {
		t.Errorf("expected caroline first, got %q", got[0])
	}
}

func TestEntityIndex_EmptyQueryReturnsEmpty(t *testing.T) {
	idx := NewEntityIndex()
	got := idx.LookupTopByEmbedding(domain.Embedding{Vector: []float32{1, 0}}, 3)
	if len(got) != 0 {
		t.Errorf("empty index should return empty slice, got %+v", got)
	}
}

func TestEntityIndex_Stats(t *testing.T) {
	idx := NewEntityIndex()
	idx.Add("a", "1", 0.5, MetaKindNamed, 1)
	idx.Add("a", "2", 0.5, MetaKindNamed, 1)
	idx.Add("b", "1", 0.5, MetaKindNamed, 1)
	total, assocs, top := idx.Stats()
	if total != 2 || assocs != 3 {
		t.Errorf("want (2, 3), got (%d, %d)", total, assocs)
	}
	if len(top) != 2 {
		t.Errorf("want 2 top entries, got %d", len(top))
	}
	if top[0].Key != "a" || top[0].Count != 2 {
		t.Errorf("top should be 'a' with count 2, got %+v", top[0])
	}
}

func TestEntityIndex_LRUEvictionPerEntity(t *testing.T) {
	idx := NewEntityIndex()
	idx.SetCaps(3, 0) // cap 3 docs per entity
	for i := 0; i < 5; i++ {
		idx.Add("named:caroline", string(rune('a'+i)), 0.5, MetaKindNamed, int64(i))
	}
	docs := idx.DocsFor("named:caroline")
	if len(docs) != 3 {
		t.Errorf("per-entity LRU cap should hold at 3, got %d", len(docs))
	}
	// Oldest 2 should have been evicted; the 3 newest remain.
	gotIDs := []string{docs[0].DocID, docs[1].DocID, docs[2].DocID}
	sort.Strings(gotIDs)
	wantIDs := []string{"c", "d", "e"}
	for i, w := range wantIDs {
		if gotIDs[i] != w {
			t.Errorf("LRU cap: want id %q at pos %d, got %q", w, i, gotIDs[i])
		}
	}
}

func TestEntityIndex_SetNameEmbedding(t *testing.T) {
	idx := NewEntityIndex()
	idx.SetNameEmbedding("named:x", "X", domain.Embedding{Vector: []float32{1, 0}})
	embs := idx.SnapshotEmbeddings()
	if _, ok := embs["named:x"]; !ok {
		t.Errorf("embedding not stored")
	}
}

func TestEntityIndex_RLockUnlock(t *testing.T) {
	idx := NewEntityIndex()
	idx.SetNameEmbedding("named:x", "X", domain.Embedding{Vector: []float32{1, 0}})
	idx.RLock()
	n := len(idx.entityEmbed)
	idx.RUnlock()
	if n != 1 {
		t.Errorf("expected 1 embedding under lock, got %d", n)
	}
}

func TestIsEntityKey(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"named:caroline", true},
		{"located:src/foo.go", true},
		{"valued:2026-04-21", true},
		{"concept:adoption", true},
		{"doc-id-123", false},
		{"", false},
		{"weird:x", false},
		{"named", false},
	}
	for _, c := range cases {
		if got := IsEntityKey(c.in); got != c.want {
			t.Errorf("IsEntityKey(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestCanonicalKey(t *testing.T) {
	if k := canonicalKey(MetaKindNamed, "caroline"); k != "named:caroline" {
		t.Errorf("got %q", k)
	}
	if k := canonicalKey(MetaKindNamed, "  caroline  "); k != "named:caroline" {
		t.Errorf("whitespace should be trimmed: %q", k)
	}
	if k := canonicalKey(MetaKindNamed, ""); k != "" {
		t.Errorf("empty name should be empty key: %q", k)
	}
	if k := canonicalKey(EntityMetaKind("bogus"), "x"); k != "" {
		t.Errorf("invalid kind should be empty: %q", k)
	}
}

// silence unused-import warning for context and time.
var _ = context.Background
var _ = time.Now
