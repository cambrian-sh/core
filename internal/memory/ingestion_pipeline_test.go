package memory

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// ── test doubles ─────────────────────────────────────────────────────────────

type mockEmbedder struct {
	vec []float32
	err error
}

func (m *mockEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return m.vec, m.err
}

func makeDoc(i int) domain.ExternalDocument {
	return domain.ExternalDocument{
		SourceURI:  fmt.Sprintf("https://example.com/%d", i),
		SourceType: "file_drop",
		Title:      fmt.Sprintf("Doc %d", i),
		Body:       fmt.Sprintf("Body of document %d.", i),
		Author:     "alice",
		Timestamp:  time.Now(),
	}
}

func testAgent() *Agent {
	mgr := &MemoryManager{Embedder: &mockEmbedder{vec: []float32{0.1}}}
	return &Agent{
		Manager: mgr,
	}
}

// Cycles 1 and 2 covered Agent.EnqueueExternal (append + drop-on-full). The method is
// gone: the ingestion pipeline commits chunks to the store directly and nothing but
// these tests ever called it.

// Cycles 3 and 4 covered the ADR-0028 SceneGenerator, deleted 2026-11-05 with
// scenes (decisions/scenes-cancelled.md).

// ── Cycle 5 — IngestionManager.Enqueue returns false when queue is full ───────

func TestIngestionManager_Enqueue_ReturnsFalseWhenFull(t *testing.T) {
	t.Parallel()
	im := NewIngestionManager(&mockEmbedder{vec: []float32{0.1}}, testAgent(),
		IngestionConfig{QueueSize: 1, BatchSize: 5, Workers: 1, BatchWait: time.Second}, config.ChunkerConfig{Default: "option_c"})

	doc := makeDoc(0)
	// Fill the queue.
	im.Enqueue(context.Background(), doc)
	// Second enqueue should fail.
	if im.Enqueue(context.Background(), doc) {
		t.Error("expected Enqueue to return false when queue is full")
	}
}

func TestIngestionManager_EndToEnd_DocFlowsToChunkFacts(t *testing.T) {
	t.Parallel()
	emb := &mockEmbedder{vec: []float32{0.1, 0.2}}
	store := &captureAllStore{}
	ag := &Agent{
		Manager: &MemoryManager{Store: store, Embedder: emb},
	}

	im := NewIngestionManager(emb, ag, IngestionConfig{
		QueueSize: 100,
		BatchSize: 1, // flush after 1 doc so test doesn't wait
		Workers:   1,
		BatchWait: 50 * time.Millisecond,
	}, config.ChunkerConfig{Default: "option_c"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	im.Start(ctx)

	doc := makeDoc(0)
	if !im.Enqueue(context.Background(), doc) {
		t.Fatal("Enqueue returned false unexpectedly")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(store.facts()) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("timed out waiting for doc chunk facts to be saved")
}
