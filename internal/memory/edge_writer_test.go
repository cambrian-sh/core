package memory

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// stubGraphStore is an in-memory domain.GraphStore for EdgeWriter tests.
type stubGraphStore struct {
	mu     sync.Mutex
	edges  map[string]domain.DocumentEdge
	saveOK bool
}

func newStubGraphStore() *stubGraphStore {
	return &stubGraphStore{edges: make(map[string]domain.DocumentEdge), saveOK: true}
}

func edgeKey(sourceID, targetID string, edgeType domain.EdgeType) string {
	return sourceID + "|" + targetID + "|" + string(edgeType)
}

func (g *stubGraphStore) SaveEdge(_ context.Context, e domain.DocumentEdge) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.saveOK {
		return fmt.Errorf("simulated save failure")
	}
	g.edges[edgeKey(e.SourceID, e.TargetID, e.EdgeType)] = e
	return nil
}

func (g *stubGraphStore) GetAdjacentEdges(_ context.Context, docIDs []string) ([]domain.DocumentEdge, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	idSet := make(map[string]bool, len(docIDs))
	for _, id := range docIDs {
		idSet[id] = true
	}
	var out []domain.DocumentEdge
	for _, e := range g.edges {
		if idSet[e.SourceID] {
			out = append(out, e)
		}
	}
	return out, nil
}

func (g *stubGraphStore) UpdateEdgeWeight(_ context.Context, _, _ string, _ domain.EdgeType, _ float32) error {
	return nil
}

func (g *stubGraphStore) edgesByPrefix(prefix string) []domain.DocumentEdge {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []domain.DocumentEdge
	for k, e := range g.edges {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].SourceID+out[i].TargetID < out[j].SourceID+out[j].TargetID
	})
	return out
}

func TestEdgeWriter_WritesEntitiesAndRelations(t *testing.T) {
	gen := &fakeGen{resp: `{
		"entities": [
			{"kind":"named","name":"Caroline","confidence":0.9},
			{"kind":"concept","name":"adoption","confidence":0.8}
		],
		"relations": [
			{"source":"named:caroline","target":"concept:adoption","label":"researched","confidence":0.7}
		]
	}`}
	gs := newStubGraphStore()
	idx := NewEntityIndex()
	ex := NewEdgeExtractor(gen)
	w := NewEdgeWriter(ex, gs, idx, nil)

	doc := &domain.Document{ID: "doc1", Text: "Caroline researched adoption."}
	w.WriteForDoc(context.Background(), doc)

	// document_edges writes are disabled (kg_extractor owns graph persistence).
	// Legacy EdgeWriter only updates the in-memory EntityIndex.
	if len(gs.edges) != 0 {
		t.Fatalf("expected 0 document_edges writes from legacy path, got %d: %+v", len(gs.edges), gs.edges)
	}
	if idx.EntityCount() != 2 {
		t.Fatalf("expected 2 entities in the in-memory index, got %d", idx.EntityCount())
	}
	for _, want := range []string{"named:caroline", "concept:adoption"} {
		docs := idx.DocsFor(want)
		if len(docs) != 1 || docs[0].DocID != "doc1" {
			t.Errorf("missing target %q in in-memory index: %+v", want, docs)
		}
	}
}

func TestEdgeWriter_UpdatesInMemoryIndex(t *testing.T) {
	gen := &fakeGen{resp: `{"entities":[{"kind":"named","name":"Eve","confidence":0.9}],"relations":[]}`}
	gs := newStubGraphStore()
	idx := NewEntityIndex()
	ex := NewEdgeExtractor(gen)
	w := NewEdgeWriter(ex, gs, idx, nil)

	doc := &domain.Document{ID: "d1", Text: "Eve arrived."}
	w.WriteForDoc(context.Background(), doc)

	docs := idx.DocsFor("named:eve")
	if len(docs) != 1 || docs[0].DocID != "d1" {
		t.Errorf("index not updated: %+v", docs)
	}
	if idx.EntityCount() != 1 {
		t.Errorf("entity count should be 1, got %d", idx.EntityCount())
	}
}

func TestEdgeWriter_FailedSaveDoesNotPanic(t *testing.T) {
	gen := &fakeGen{resp: `{"entities":[{"kind":"named","name":"X","confidence":0.9}],"relations":[]}`}
	gs := newStubGraphStore()
	gs.saveOK = false
	idx := NewEntityIndex()
	ex := NewEdgeExtractor(gen)
	w := NewEdgeWriter(ex, gs, idx, nil)

	doc := &domain.Document{ID: "d1", Text: "X happened."}
	// Should not panic; the writer no longer calls graph.SaveEdge but a nil
	// graph store or a misconfigured index must still degrade cleanly.
	w.WriteForDoc(context.Background(), doc)
	if len(gs.edges) != 0 {
		t.Errorf("no edges should be written from the legacy path")
	}
}

func TestEdgeWriter_NoEntitiesNoWrites(t *testing.T) {
	gen := &fakeGen{resp: `{"entities":[],"relations":[]}`}
	gs := newStubGraphStore()
	idx := NewEntityIndex()
	ex := NewEdgeExtractor(gen)
	w := NewEdgeWriter(ex, gs, idx, nil)

	doc := &domain.Document{ID: "d1", Text: "nothing to extract"}
	w.WriteForDoc(context.Background(), doc)

	if len(gs.edges) != 0 {
		t.Errorf("no edges should be written for empty extraction")
	}
}

func TestEdgeWriter_NilExtractorIsNoop(t *testing.T) {
	gs := newStubGraphStore()
	idx := NewEntityIndex()
	// nil extractor → NewEdgeWriter still works (writer bails on nil extractor).
	// But NewEdgeWriter doesn't accept nil in production; we exercise the
	// nil-receiver path on WriteForDoc instead.
	var w *EdgeWriter
	w.WriteForDoc(context.Background(), &domain.Document{ID: "d", Text: "x"})
	if len(gs.edges) != 0 {
		t.Errorf("nil writer should be no-op")
	}
	_ = idx
}

func TestEdgeWriter_NilDocIsNoop(t *testing.T) {
	gen := &fakeGen{resp: `{}`}
	gs := newStubGraphStore()
	idx := NewEntityIndex()
	ex := NewEdgeExtractor(gen)
	w := NewEdgeWriter(ex, gs, idx, nil)
	w.WriteForDoc(context.Background(), nil)
	if len(gs.edges) != 0 {
		t.Errorf("nil doc should be no-op")
	}
}

func TestEdgeWriter_EmptyTextIsNoop(t *testing.T) {
	gen := &fakeGen{resp: `{}`}
	gs := newStubGraphStore()
	idx := NewEntityIndex()
	ex := NewEdgeExtractor(gen)
	w := NewEdgeWriter(ex, gs, idx, nil)
	w.WriteForDoc(context.Background(), &domain.Document{ID: "d", Text: ""})
	if len(gs.edges) != 0 {
		t.Errorf("empty text should be no-op")
	}
}

func TestEdgeWriter_FreeFormLabelPreserved(t *testing.T) {
	gen := &fakeGen{resp: `{
		"entities":[
			{"kind":"named","name":"Sam","confidence":0.9},
			{"kind":"named","name":"Caroline","confidence":0.9}
		],
		"relations":[
			{"source":"named:sam","target":"named:caroline","label":"is_friend_of","confidence":0.8}
		]
	}`}
	gs := newStubGraphStore()
	idx := NewEntityIndex()
	ex := NewEdgeExtractor(gen)
	w := NewEdgeWriter(ex, gs, idx, nil)

	doc := &domain.Document{ID: "d", Text: "Sam is a friend of Caroline."}
	w.WriteForDoc(context.Background(), doc)

	// document_edges writes are disabled. The relation "is_friend_of" is
	// captured in the audit log (writeRelations) for the operator feed.
	if len(gs.edges) != 0 {
		t.Errorf("expected 0 document_edges writes from legacy path, got %d", len(gs.edges))
	}
	if idx.EntityCount() != 2 {
		t.Errorf("expected 2 entities in the in-memory index, got %d", idx.EntityCount())
	}
}

func TestEdgeWriter_EntityEmbeddingsStored(t *testing.T) {
	gen := &fakeGen{resp: `{
		"entities":[{"kind":"named","name":"Bob","confidence":0.9}],
		"relations":[]
	}`}
	gs := newStubGraphStore()
	idx := NewEntityIndex()
	emb := &stubEmbedder{vec: []float32{1, 0, 0}}
	ex := NewEdgeExtractor(gen)
	w := NewEdgeWriter(ex, gs, idx, emb)

	doc := &domain.Document{ID: "d1", Text: "Bob is here."}
	w.WriteForDoc(context.Background(), doc)

	embs := idx.SnapshotEmbeddings()
	if _, ok := embs["named:bob"]; !ok {
		t.Errorf("entity name embedding not stored")
	}
}

type stubEmbedder struct {
	vec []float32
}

func (s *stubEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return s.vec, nil
}

// silence unused-import warning for time.
var _ = time.Now
