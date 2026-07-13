package memory

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// testBatcherStore wraps a fakeChunkTripletsStore and counts saves; used to
// verify the batcher persists what the parser extracts.
type testBatcherStore struct {
	*fakeChunkTripletsStore
	saves uint64
}

func (s *testBatcherStore) SaveChunkTriplets(ctx context.Context, chunkID string, triplets []ChunkTriplet) error {
	atomic.AddUint64(&s.saves, 1)
	return s.fakeChunkTripletsStore.SaveChunkTriplets(ctx, chunkID, triplets)
}

// streamingBatchGen is a fakeGen that returns a fixed response, with optional
// streaming support. Used to verify the batcher routes through GenerateStream
// when the Generator advertises it.
type streamingBatchGen struct {
	mu       sync.Mutex
	resp     string
	got      []string
	calls    uint64
	streamed uint64
}

func (g *streamingBatchGen) Generate(_ context.Context, prompt string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.got = append(g.got, prompt)
	atomic.AddUint64(&g.calls, 1)
	return g.resp, nil
}

func (g *streamingBatchGen) GenerateStream(_ context.Context, prompt string) (<-chan domain.StreamChunk, error) {
	g.mu.Lock()
	g.got = append(g.got, prompt)
	g.mu.Unlock()
	atomic.AddUint64(&g.calls, 1)
	atomic.AddUint64(&g.streamed, 1)
	ch := make(chan domain.StreamChunk, 2)
	ch <- domain.StreamChunk{Text: g.resp}
	ch <- domain.StreamChunk{IsFinal: true}
	close(ch)
	return ch, nil
}

func newTestBatcherLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(testWriter{t: nil}, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(b []byte) (int, error) {
	if w.t != nil && strings.Contains(string(b), "queue full") {
		w.t.Logf("batcher: %s", strings.TrimSpace(string(b)))
	}
	return len(b), nil
}

func TestParseBatchedTripletResponse_SplitsPerChunk(t *testing.T) {
	resp := `Triplets 1: <caroline##researched##quantum>$$<caroline##lives in##melbourne>
Triplets 2: <bob##works at##ibm>$$<bob##colleague of##alice>
Triplets 3: <eve##spoke to##frank>`
	got := parseBatchedTripletResponse(resp, 3)
	if len(got) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(got))
	}
	if len(got[0]) != 2 || got[0][0].H != "caroline" || got[0][0].T != "quantum" {
		t.Errorf("chunk 0 wrong: %+v", got[0])
	}
	if len(got[1]) != 2 || got[1][1].H != "bob" || got[1][1].T != "alice" {
		t.Errorf("chunk 1 wrong: %+v", got[1])
	}
	if len(got[2]) != 1 || got[2][0].T != "frank" {
		t.Errorf("chunk 2 wrong: %+v", got[2])
	}
}

func TestParseBatchedTripletResponse_HandlesMissingMarkers(t *testing.T) {
	// LLM completely misses the format; treat as chunk 0.
	resp := `<caroline##researched##quantum>`
	got := parseBatchedTripletResponse(resp, 2)
	if len(got[0]) != 1 {
		t.Errorf("expected 1 triplet on chunk 0, got %+v", got[0])
	}
}

func TestChunkTripletsBatcher_PersistsAfterDrain(t *testing.T) {
	store := &testBatcherStore{fakeChunkTripletsStore: newFakeChunkTripletsStore()}
	gen := &streamingBatchGen{
		resp: `Triplets 1: <caroline##researched##quantum>$$<caroline##lives in##melbourne>
Triplets 2: <bob##works at##ibm>`,
	}
	b := NewChunkTripletsBatcher(gen, store, ChunkTripletsBatcherConfig{
		QueueSize:  8, BatchSize: 2, MaxIdle: 50 * time.Millisecond, LLMTimeout: 5 * time.Second,
	})
	b.Start(context.Background())
	defer b.Stop()

	b.Enqueue(&domain.Document{ID: "c1", Text: "caroline researched quantum and lives in Melbourne."})
	b.Enqueue(&domain.Document{ID: "c2", Text: "bob works at IBM."})

	// Wait for the batcher to drain.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		e, _, dr, _, _ := b.Stats()
		if e >= 2 && dr >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	e, _, dr, llm, tr := b.Stats()
	if e < 2 {
		t.Errorf("expected enqueued >= 2, got %d", e)
	}
	if dr < 2 {
		t.Errorf("expected drained >= 2, got %d", dr)
	}
	if llm < 1 {
		t.Errorf("expected at least 1 LLM call, got %d", llm)
	}
	if tr < 3 {
		t.Errorf("expected 3 triplets persisted (2 from c1 + 1 from c2), got %d", tr)
	}
	// Verify the store has the triplets.
	c1, _ := store.ForChunk(context.Background(), "c1")
	if len(c1) != 2 {
		t.Errorf("expected 2 triplets for c1, got %d: %+v", len(c1), c1)
	}
	c2, _ := store.ForChunk(context.Background(), "c2")
	if len(c2) != 1 {
		t.Errorf("expected 1 triplet for c2, got %d: %+v", len(c2), c2)
	}
}

func TestChunkTripletsBatcher_UsesStreaming(t *testing.T) {
	store := &testBatcherStore{fakeChunkTripletsStore: newFakeChunkTripletsStore()}
	gen := &streamingBatchGen{
		resp: `Triplets 1: <caroline##researched##quantum>`,
	}
	b := NewChunkTripletsBatcher(gen, store, ChunkTripletsBatcherConfig{
		QueueSize:  8, BatchSize: 1, MaxIdle: 50 * time.Millisecond, LLMTimeout: 5 * time.Second,
	})
	b.Start(context.Background())
	b.Enqueue(&domain.Document{ID: "c1", Text: "caroline researched quantum."})

	// Wait for at least 1 LLM call.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadUint64(&gen.calls) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	b.Stop()

	if atomic.LoadUint64(&gen.streamed) == 0 {
		t.Errorf("expected batcher to use GenerateStream when available")
	}
}

func TestChunkTripletsBatcher_DropsOnQueueFull(t *testing.T) {
	// Use a slow LLM (never returns) so the queue fills and Enqueue drops.
	store := &testBatcherStore{fakeChunkTripletsStore: newFakeChunkTripletsStore()}
	blocking := &blockingGen{}
	b := NewChunkTripletsBatcher(blocking, store, ChunkTripletsBatcherConfig{
		QueueSize:  2, BatchSize: 1, MaxIdle: 50 * time.Millisecond, LLMTimeout: 100 * time.Millisecond,
	})
	b.Start(context.Background())
	defer b.Stop()

	for i := 0; i < 10; i++ {
		b.Enqueue(&domain.Document{ID: "c" + itoa(i), Text: "x"})
	}
	_, dropped, _, _, _ := b.Stats()
	if dropped == 0 {
		t.Errorf("expected some drops with queue=2 and 10 enqueues, got dropped=%d", dropped)
	}
}

type blockingGen struct{}

func (b *blockingGen) Generate(ctx context.Context, _ string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

// streaming support
func (b *blockingGen) GenerateStream(ctx context.Context, _ string) (<-chan domain.StreamChunk, error) {
	ch := make(chan domain.StreamChunk)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

func TestBuildBatchTripletPrompt_HasMarkersAndExamples(t *testing.T) {
	p := buildBatchTripletPrompt([]string{"chunk 1 text", "chunk 2 text"})
	for _, want := range []string{
		"Triplets 1:",
		"Triplets 2:",
		"Chunk 1:",
		"Chunk 2:",
		"chunk 1 text",
		"chunk 2 text",
		"<Scott Derrickson##born in##1966>", // real example in the prompt
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q\n--- prompt ---\n%s", want, p)
		}
	}
}

// TestChunkTripletsBatcher_RespectsBatchSize is the regression test for the
// "drain sends ALL pending" bug. With 50 chunks enqueued but BatchSize=4, the
// batcher should make 13 LLM calls (4+4+4+4+4+4+4+4+4+4+4+4+2), not 1 call
// with 50 chunks. The LLM records every prompt it sees; the test counts
// calls and checks the prompt size of each.
func TestChunkTripletsBatcher_RespectsBatchSize(t *testing.T) {
	store := &testBatcherStore{fakeChunkTripletsStore: newFakeChunkTripletsStore()}
	// gen returns one triplet per chunk (positional).
	gen := &countingBatchGen{respTriplets: 1}
	b := NewChunkTripletsBatcher(gen, store, ChunkTripletsBatcherConfig{
		QueueSize:  64, BatchSize: 4, MaxIdle: 20 * time.Millisecond, LLMTimeout: 5 * time.Second,
	})
	b.Start(context.Background())

	const total = 50
	for i := 0; i < total; i++ {
		b.Enqueue(&domain.Document{ID: "c" + itoa(i), Text: "some text"})
	}

	// Wait until all 50 are enqueued AND drained.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		enq, _, dr, _, _ := b.Stats()
		if enq >= total && dr >= total {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	b.Stop()

	enq, _, dr, calls, _ := b.Stats()
	if enq < total {
		t.Errorf("expected enqueued=%d, got %d", total, enq)
	}
	if dr < total {
		t.Errorf("expected drained=%d, got %d", total, dr)
	}
	// Expected: 50 / 4 = 13 LLM calls (last batch has 2)
	expectedCalls := (total + 3) / 4 // ceiling division
	if int(calls) != expectedCalls {
		t.Errorf("expected %d LLM calls (BatchSize=4, %d chunks), got %d",
			expectedCalls, total, calls)
	}
	// Verify no prompt exceeded BatchSize=4
	maxBatch := 0
	for _, n := range gen.batchSizes {
		if n > maxBatch {
			maxBatch = n
		}
	}
	if maxBatch > 4 {
		t.Errorf("LLM prompt exceeded BatchSize=4: max observed batch was %d", maxBatch)
	}
}

// countingBatchGen is a fakeGen that records the size of each prompt it
// receives. The response is N triplets (one per chunk, positional).
type countingBatchGen struct {
	mu          sync.Mutex
	batchSizes  []int
	respTriplets int
}

func (g *countingBatchGen) Generate(_ context.Context, prompt string) (string, error) {
	// Count "Chunk " markers in the prompt to determine batch size.
	g.mu.Lock()
	defer g.mu.Unlock()
	n := strings.Count(prompt, "Chunk ")
	g.batchSizes = append(g.batchSizes, n)
	// Build response: "Triplets 1: ..." repeated n times
	var sb strings.Builder
	for i := 1; i <= n; i++ {
		sb.WriteString("Triplets ")
		sb.WriteString(itoa(i))
		sb.WriteString(": <a##b##c>\n")
	}
	return sb.String(), nil
}

func (g *countingBatchGen) GenerateStream(_ context.Context, prompt string) (<-chan domain.StreamChunk, error) {
	g.mu.Lock()
	n := strings.Count(prompt, "Chunk ")
	g.batchSizes = append(g.batchSizes, n)
	g.mu.Unlock()
	var sb strings.Builder
	for i := 1; i <= n; i++ {
		sb.WriteString("Triplets ")
		sb.WriteString(itoa(i))
		sb.WriteString(": <a##b##c>\n")
	}
	ch := make(chan domain.StreamChunk, 2)
	ch <- domain.StreamChunk{Text: sb.String()}
	ch <- domain.StreamChunk{IsFinal: true}
	close(ch)
	return ch, nil
}
