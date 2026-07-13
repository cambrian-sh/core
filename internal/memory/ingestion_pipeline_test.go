package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// ── test doubles ─────────────────────────────────────────────────────────────

type mockLLMGen struct {
	response string
	err      error
	calls    int
}

func (m *mockLLMGen) Generate(_ context.Context, _ string) (string, error) {
	m.calls++
	if m.err != nil {
		return "", m.err
	}
	return m.response, nil
}

type mockEmbedder struct {
	vec []float32
	err error
}

func (m *mockEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return m.vec, m.err
}

func scenesJSON(n int) string {
	scenes := make([]string, n)
	for i := range scenes {
		scenes[i] = fmt.Sprintf("Scene for document %d", i+1)
	}
	b, _ := json.Marshal(scenes)
	return string(b)
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
		Manager:    mgr,
		pendingCap: 256,
	}
}

// ── Cycle 1 — Agent.EnqueueExternal appends items to pendingItems ─────────────

func TestAgent_EnqueueExternal_AppendsItems(t *testing.T) {
	a := testAgent()
	items := []pendingItem{
		{Embedding: []float32{0.1}, Doc: &domain.Document{ID: "d1"}},
		{Embedding: []float32{0.2}, Doc: &domain.Document{ID: "d2"}},
	}
	if err := a.EnqueueExternal(context.Background(), items); err != nil {
		t.Fatalf("EnqueueExternal: %v", err)
	}
	a.pendingMu.RLock()
	defer a.pendingMu.RUnlock()
	if len(a.pendingItems) != 2 {
		t.Errorf("expected 2 pendingItems, got %d", len(a.pendingItems))
	}
}

// ── Cycle 2 — Agent.EnqueueExternal drops when pendingCap is reached ─────────

func TestAgent_EnqueueExternal_DropsOnFull(t *testing.T) {
	a := testAgent()
	a.pendingCap = 1
	a.pendingItems = []pendingItem{{Doc: &domain.Document{ID: "existing"}}}

	overflow := []pendingItem{{Embedding: []float32{0.9}, Doc: &domain.Document{ID: "overflow"}}}
	_ = a.EnqueueExternal(context.Background(), overflow)

	a.pendingMu.RLock()
	defer a.pendingMu.RUnlock()
	if len(a.pendingItems) != 1 {
		t.Errorf("expected 1 item (overflow dropped), got %d", len(a.pendingItems))
	}
}

// ── Cycle 3 — SceneGenerator.Generate calls LLM once for 5 docs ──────────────

func TestSceneGenerator_Generate_BatchOf5_OneLLMCall(t *testing.T) {
	gen := &mockLLMGen{response: scenesJSON(5)}
	sg := NewSceneGenerator(gen)
	batch := make([]domain.ExternalDocument, 5)
	for i := range batch {
		batch[i] = makeDoc(i)
	}

	scenes, err := sg.Generate(context.Background(), batch)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(scenes) != 5 {
		t.Fatalf("expected 5 scenes, got %d", len(scenes))
	}
	if gen.calls != 1 {
		t.Errorf("expected 1 LLM call, got %d", gen.calls)
	}
	for i, s := range scenes {
		if s == "" {
			t.Errorf("scene %d is empty", i)
		}
	}
}

// ── Cycle 4 — SceneGenerator.Generate falls back to placeholders on LLM error ─

func TestSceneGenerator_Generate_LLMError_FallbackPlaceholders(t *testing.T) {
	gen := &mockLLMGen{err: fmt.Errorf("timeout")}
	sg := NewSceneGenerator(gen)
	batch := []domain.ExternalDocument{makeDoc(0), makeDoc(1)}

	scenes, err := sg.Generate(context.Background(), batch)
	if err != nil {
		t.Fatalf("Generate should not propagate LLM errors: %v", err)
	}
	if len(scenes) != 2 {
		t.Fatalf("expected 2 fallback scenes, got %d", len(scenes))
	}
	for i, s := range scenes {
		if !strings.Contains(s, "scene generation pending") {
			t.Errorf("scene %d: expected fallback placeholder, got %q", i, s)
		}
	}
}

// ── Cycle 5 — IngestionManager.Enqueue returns false when queue is full ───────

func TestIngestionManager_Enqueue_ReturnsFalseWhenFull(t *testing.T) {
	sg := NewSceneGenerator(&mockLLMGen{response: scenesJSON(5)})
	im := NewIngestionManager(sg, &mockEmbedder{vec: []float32{0.1}}, testAgent(),
		IngestionConfig{QueueSize: 1, BatchSize: 5, Workers: 1, BatchWait: time.Second})

	doc := makeDoc(0)
	// Fill the queue.
	im.Enqueue(doc)
	// Second enqueue should fail.
	if im.Enqueue(doc) {
		t.Error("expected Enqueue to return false when queue is full")
	}
}

func TestIngestionManager_EndToEnd_DocFlowsToChunkFacts(t *testing.T) {
	gen := &mockLLMGen{response: scenesJSON(5)}
	emb := &mockEmbedder{vec: []float32{0.1, 0.2}}
	store := &captureAllStore{}
	ag := &Agent{
		Manager:    &MemoryManager{Store: store, Embedder: emb},
		pendingCap: 256,
	}

	sg := NewSceneGenerator(gen)
	im := NewIngestionManager(sg, emb, ag, IngestionConfig{
		QueueSize: 100,
		BatchSize: 1, // flush after 1 doc so test doesn't wait
		Workers:   1,
		BatchWait: 50 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	im.Start(ctx)

	doc := makeDoc(0)
	if !im.Enqueue(doc) {
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
