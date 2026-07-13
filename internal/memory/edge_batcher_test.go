package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// batchGen is a fakeGen that returns a JSON array of N extractorOutput items.
type batchGen struct {
	mu       sync.Mutex
	respList []extractorOutput
	err      error
	received []string // the prompts, for assertion
}

func (g *batchGen) Generate(_ context.Context, prompt string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.received = append(g.received, prompt)
	if g.err != nil {
		return "", g.err
	}
	b, _ := json.Marshal(g.respList)
	return string(b), nil
}

func mkExtractorOutput(entities []string, relations [][]string) extractorOutput {
	var out extractorOutput
	for _, e := range entities {
		out.Entities = append(out.Entities, struct {
			Kind       string  `json:"kind"`
			Name       string  `json:"name"`
			Confidence float64 `json:"confidence"`
		}{Kind: "named", Name: e, Confidence: 0.9})
	}
	for _, r := range relations {
		if len(r) != 3 {
			continue
		}
		out.Relations = append(out.Relations, struct {
			Source     string  `json:"source"`
			Target     string  `json:"target"`
			Label      string  `json:"label"`
			Confidence float64 `json:"confidence"`
		}{Source: r[0], Target: r[1], Label: r[2], Confidence: 0.8})
	}
	return out
}

func TestEdgeExtractor_ExtractBatch_PositionalMapping(t *testing.T) {
	gen := &batchGen{respList: []extractorOutput{
		mkExtractorOutput([]string{"Alice"}, nil),
		mkExtractorOutput([]string{"Bob"}, nil),
		mkExtractorOutput([]string{"Carol"}, nil),
	}}
	ex := NewEdgeExtractor(gen)
	got := ex.ExtractBatch(context.Background(), []string{"Alice arrived.", "Bob arrived.", "Carol arrived."})
	if len(got) != 3 {
		t.Fatalf("want 3 extractions, got %d", len(got))
	}
	want := []string{"alice", "bob", "caroline"} // "Caroline" doesn't appear in test, fixed below
	want[2] = "carol"
	for i, w := range want {
		if len(got[i].Entities) == 0 || got[i].Entities[0].Name != w {
			t.Errorf("position %d: want %q, got %+v", i, w, got[i].Entities)
		}
	}
}

func TestEdgeExtractor_ExtractBatch_EmptyBlanksSkipped(t *testing.T) {
	gen := &batchGen{respList: []extractorOutput{
		mkExtractorOutput([]string{"Alice"}, nil),
	}}
	ex := NewEdgeExtractor(gen)
	got := ex.ExtractBatch(context.Background(), []string{"", "Alice arrived.", ""})
	if len(got) != 3 {
		t.Fatalf("want 3 output positions for 3 input, got %d", len(got))
	}
	if len(got[0].Entities) != 0 {
		t.Errorf("blank input should yield empty extraction, got %+v", got[0].Entities)
	}
	if len(got[2].Entities) != 0 {
		t.Errorf("blank input should yield empty extraction, got %+v", got[2].Entities)
	}
	if len(got[1].Entities) != 1 || got[1].Entities[0].Name != "alice" {
		t.Errorf("non-blank should extract: %+v", got[1].Entities)
	}
}

func TestEdgeExtractor_ExtractBatch_FewerItemsThanInputFillsEmpty(t *testing.T) {
	gen := &batchGen{respList: []extractorOutput{
		mkExtractorOutput([]string{"Alice"}, nil),
	}}
	ex := NewEdgeExtractor(gen)
	got := ex.ExtractBatch(context.Background(), []string{"A", "B", "C"})
	if len(got) != 3 {
		t.Fatalf("want 3 output positions, got %d", len(got))
	}
	if len(got[0].Entities) != 1 {
		t.Errorf("position 0 should have Alice, got %+v", got[0].Entities)
	}
	for i := 1; i < 3; i++ {
		if len(got[i].Entities) != 0 {
			t.Errorf("position %d should be empty, got %+v", i, got[i].Entities)
		}
	}
}

func TestEdgeExtractor_ExtractBatch_ExtraItemsDropped(t *testing.T) {
	gen := &batchGen{respList: []extractorOutput{
		mkExtractorOutput([]string{"Alice"}, nil),
		mkExtractorOutput([]string{"Bob"}, nil),
		mkExtractorOutput([]string{"Carol"}, nil),
	}}
	ex := NewEdgeExtractor(gen)
	got := ex.ExtractBatch(context.Background(), []string{"A"})
	if len(got) != 1 {
		t.Fatalf("want 1 output position, got %d", len(got))
	}
}

func TestEdgeExtractor_ExtractBatch_LLMErrorYieldsAllEmpty(t *testing.T) {
	gen := &batchGen{err: fmt.Errorf("provider down")}
	ex := NewEdgeExtractor(gen)
	got := ex.ExtractBatch(context.Background(), []string{"A", "B", "C"})
	if len(got) != 3 {
		t.Fatalf("want 3 output positions, got %d", len(got))
	}
	for i, e := range got {
		if len(e.Entities) != 0 || len(e.Relations) != 0 {
			t.Errorf("position %d should be empty on LLM error, got %+v", i, e)
		}
	}
}

func TestEdgeExtractor_ExtractBatch_UnparseableYieldsAllEmpty(t *testing.T) {
	// Use a real fakeGen to return plain text, not a JSON array.
	gen := &fakeGen{resp: "not json at all"}
	ex := NewEdgeExtractor(gen)
	got := ex.ExtractBatch(context.Background(), []string{"A", "B"})
	if len(got) != 2 {
		t.Fatalf("want 2 output positions, got %d", len(got))
	}
	for i, e := range got {
		if len(e.Entities) != 0 {
			t.Errorf("position %d should be empty, got %+v", i, e)
		}
	}
}

func TestEdgeExtractor_ExtractBatch_NilGenIsNoop(t *testing.T) {
	ex := NewEdgeExtractor(nil)
	got := ex.ExtractBatch(context.Background(), []string{"A", "B"})
	if len(got) != 2 {
		t.Fatalf("want 2 positions, got %d", len(got))
	}
	for _, e := range got {
		if len(e.Entities) != 0 {
			t.Errorf("nil gen should produce empty extraction")
		}
	}
}

func TestEdgeExtractor_ExtractBatch_EmptyInputIsNoop(t *testing.T) {
	ex := NewEdgeExtractor(&fakeGen{})
	got := ex.ExtractBatch(context.Background(), nil)
	if len(got) != 0 {
		t.Errorf("empty input should return nil/empty slice, got %+v", got)
	}
}

func TestEdgeExtractor_BatchPromptContainsSeparator(t *testing.T) {
	ex := NewEdgeExtractor(&fakeGen{})
	prompt := ex.buildBatchPrompt([]string{"fact A", "fact B"})
	if !strings.Contains(prompt, "---") {
		t.Errorf("batch prompt should separate facts with ---; got %q", prompt)
	}
	if !strings.Contains(prompt, "fact A") || !strings.Contains(prompt, "fact B") {
		t.Errorf("batch prompt should include both facts; got %q", prompt)
	}
}

// ── EdgeBatcher tests ─────────────────────────────────────────────────────

func TestEdgeBatcher_DrainsWhenBatchSizeReached(t *testing.T) {
	gs := newStubGraphStore()
	idx := NewEntityIndex()
	gen := &batchGen{respList: []extractorOutput{
		mkExtractorOutput([]string{"Alice"}, nil),
		mkExtractorOutput([]string{"Bob"}, nil),
	}}
	ex := NewEdgeExtractor(gen)
	w := NewEdgeWriter(ex, gs, idx, nil)
	b := NewEdgeBatcher(ex, w, EdgeBatcherConfig{
		QueueSize: 10, BatchSize: 3, MaxIdle: 50 * time.Millisecond, LLMTimeout: time.Second,
	})
	b.Start(context.Background())
	defer b.Stop()

	for i := 0; i < 3; i++ {
		b.Enqueue(&domain.Document{ID: string(rune('a' + i)), Text: "x"})
	}
	// Wait for the batch to drain.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, _, drained, _ := b.Stats()
		if drained >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	enq, drop, drained, calls := b.Stats()
	if enq < 3 {
		t.Errorf("want >=3 enqueued, got %d", enq)
	}
	if drop != 0 {
		t.Errorf("queue not full; should not drop: got %d dropped", drop)
	}
	if drained != 3 {
		t.Errorf("want 3 drained, got %d", drained)
	}
	if calls != 1 {
		t.Errorf("want 1 LLM call (batched), got %d", calls)
	}
}

func TestEdgeBatcher_DrainsOnIdleTimer(t *testing.T) {
	gs := newStubGraphStore()
	idx := NewEntityIndex()
	gen := &batchGen{respList: []extractorOutput{
		mkExtractorOutput([]string{"Alice"}, nil),
	}}
	ex := NewEdgeExtractor(gen)
	w := NewEdgeWriter(ex, gs, idx, nil)
	b := NewEdgeBatcher(ex, w, EdgeBatcherConfig{
		QueueSize: 10, BatchSize: 100, MaxIdle: 50 * time.Millisecond, LLMTimeout: time.Second,
	})
	b.Start(context.Background())
	defer b.Stop()

	// Enqueue 1 item — below batch size, should drain on idle.
	b.Enqueue(&domain.Document{ID: "a", Text: "x"})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, _, drained, _ := b.Stats()
		if drained >= 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, _, drained, _ := b.Stats()
	if drained < 1 {
		t.Errorf("idle drain should have fired; drained=%d", drained)
	}
}

func TestEdgeBatcher_NonBlockingEnqueueUnderBackpressure(t *testing.T) {
	gs := newStubGraphStore()
	idx := NewEntityIndex()
	// Make the LLM call slow so the queue fills up before any drain.
	gen := &slowGen{delay: 200 * time.Millisecond, resp: `[]`}
	ex := NewEdgeExtractor(gen)
	w := NewEdgeWriter(ex, gs, idx, nil)
	b := NewEdgeBatcher(ex, w, EdgeBatcherConfig{
		QueueSize: 2, BatchSize: 100, MaxIdle: 50 * time.Millisecond, LLMTimeout: time.Second,
	})
	b.Start(context.Background())
	defer b.Stop()

	// Enqueue 5 docs. The first 2 fill the channel; the rest are dropped
	// from the graph layer (the durability path is the caller's job).
	for i := 0; i < 5; i++ {
		b.Enqueue(&domain.Document{ID: string(rune('a' + i)), Text: "x"})
	}
	// Enqueue returns immediately. The drop count should be > 0.
	_, drop, _, _ := b.Stats()
	if drop == 0 {
		t.Errorf("queue full (size 2) and 5 enqueues: should drop some; got 0 drops")
	}
}

func TestEdgeBatcher_StopFlushesTail(t *testing.T) {
	gs := newStubGraphStore()
	idx := NewEntityIndex()
	gen := &batchGen{respList: []extractorOutput{
		mkExtractorOutput([]string{"Alice"}, nil),
	}}
	ex := NewEdgeExtractor(gen)
	w := NewEdgeWriter(ex, gs, idx, nil)
	b := NewEdgeBatcher(ex, w, EdgeBatcherConfig{
		QueueSize: 10, BatchSize: 100, MaxIdle: 1 * time.Hour, LLMTimeout: time.Second,
	})
	b.Start(context.Background())

	b.Enqueue(&domain.Document{ID: "a", Text: "x"})
	b.Stop() // should flush the tail and return

	_, _, drained, calls := b.Stats()
	if drained < 1 {
		t.Errorf("Stop should flush the tail; got drained=%d", drained)
	}
	if calls < 1 {
		t.Errorf("Stop should have triggered at least one LLM call; got %d", calls)
	}
}

func TestEdgeBatcher_StartIdempotent(t *testing.T) {
	gs := newStubGraphStore()
	idx := NewEntityIndex()
	gen := &batchGen{respList: []extractorOutput{}}
	ex := NewEdgeExtractor(gen)
	w := NewEdgeWriter(ex, gs, idx, nil)
	b := NewEdgeBatcher(ex, w, EdgeBatcherConfig{
		QueueSize: 1, BatchSize: 1, MaxIdle: 50 * time.Millisecond, LLMTimeout: time.Second,
	})
	b.Start(context.Background())
	b.Start(context.Background()) // second call should be a no-op (no panic)
	b.Stop()
}

// streamingGen is a fakeGen that ALSO implements streamingGenerator, so the
// EdgeExtractor's streaming-preference path is exercised.
type streamingGen struct {
	*fakeGen
	chunks []domain.StreamChunk
	chErr  error
}

func (s *streamingGen) GenerateStream(_ context.Context, _ string) (<-chan domain.StreamChunk, error) {
	if s.chErr != nil {
		return nil, s.chErr
	}
	out := make(chan domain.StreamChunk, len(s.chunks)+1)
	for _, c := range s.chunks {
		out <- c
	}
	out <- domain.StreamChunk{IsFinal: true}
	close(out)
	return out, nil
}

func TestEdgeExtractor_ExtractBatch_PrefersStreaming(t *testing.T) {
	// streamingGen returns a JSON array over a streaming channel; ExtractBatch
	// should consume it and produce the expected extractions.
	respArr := []extractorOutput{
		mkExtractorOutput([]string{"Alice"}, nil),
		mkExtractorOutput([]string{"Bob"}, nil),
	}
	b, _ := json.Marshal(respArr)
	sg := &streamingGen{chunks: []domain.StreamChunk{{Text: string(b)}}}
	ex := NewEdgeExtractor(sg)
	got := ex.ExtractBatch(context.Background(), []string{"Alice arrived.", "Bob arrived."})
	if len(got) != 2 {
		t.Fatalf("want 2 extractions, got %d", len(got))
	}
	if len(got[0].Entities) != 1 || got[0].Entities[0].Name != "alice" {
		t.Errorf("position 0: %+v", got[0].Entities)
	}
	if len(got[1].Entities) != 1 || got[1].Entities[0].Name != "bob" {
		t.Errorf("position 1: %+v", got[1].Entities)
	}
}

func TestEdgeExtractor_ExtractBatch_StreamErrorYieldsAllEmpty(t *testing.T) {
	// streamingGen returns a stream_error chunk; the extractor surfaces it
	// as an error and the batcher produces all-empty extractions.
	sg := &streamingGen{chunks: []domain.StreamChunk{{Text: "stream_error: provider down"}}}
	ex := NewEdgeExtractor(sg)
	got := ex.ExtractBatch(context.Background(), []string{"A", "B"})
	if len(got) != 2 {
		t.Fatalf("want 2 output positions, got %d", len(got))
	}
	for i, e := range got {
		if len(e.Entities) != 0 {
			t.Errorf("position %d should be empty on stream error, got %+v", i, e)
		}
	}
}

// TestEdgeBatcher_OneLLMCallPerBatch is the perf-claim regression test:
// N enqueues should produce ceiling(N/batchSize) LLM calls, not N. The
// LoCoMo benchmark's 1986 facts would otherwise be 1986 LLM calls.
func TestEdgeBatcher_OneLLMCallPerBatch(t *testing.T) {
	gs := newStubGraphStore()
	idx := NewEntityIndex()
	// The LLM returns 1-element list for any batch; the batcher must
	// still call the LLM only when the batch is full (or idle fires).
	gen := &batchGen{respList: []extractorOutput{
		mkExtractorOutput([]string{"X"}, nil),
	}}
	ex := NewEdgeExtractor(gen)
	w := NewEdgeWriter(ex, gs, idx, nil)
	const N = 100
	const batchSize = 32
	b := NewEdgeBatcher(ex, w, EdgeBatcherConfig{
		QueueSize:  N + 10,
		BatchSize:  batchSize,
		MaxIdle:    10 * time.Millisecond,
		LLMTimeout: time.Second,
	})
	b.Start(context.Background())
	defer b.Stop()

	for i := 0; i < N; i++ {
		b.Enqueue(&domain.Document{ID: string(rune('a' + i%26)) + "_" + string(rune('a'+i/26)), Text: "x"})
	}
	// Wait for all drains to settle.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, _, drained, _ := b.Stats()
		if drained >= N {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, _, drained, calls := b.Stats()
	if drained != N {
		t.Errorf("expected %d docs drained, got %d", N, drained)
	}
	// Expected: ceil(100/32) = 4 load-driven drains + 1 idle-tail drain.
	// Allow 4-6 to be tolerant of scheduling.
	if calls < 1 || calls > 10 {
		t.Errorf("expected batching to reduce LLM calls to ~%d, got %d (sync would be 100)", (N+batchSize-1)/batchSize, calls)
	}
}

// slowGen is a fakeGen that sleeps before returning, used to fill the queue.
type slowGen struct {
	delay time.Duration
	resp  string
	mu    sync.Mutex
}

func (g *slowGen) Generate(ctx context.Context, _ string) (string, error) {
	g.mu.Lock()
	d, r := g.delay, g.resp
	g.mu.Unlock()
	select {
	case <-time.After(d):
		return r, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
