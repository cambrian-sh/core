package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Ollama /api/embeddings + /api/embed failure modes
// (T-20261031-documented-production-failure-modes-have-no, "embed endpoint
// slowness / batch endpoint fallback"). The endpoint is an httptest server; the
// embedder, the shared retrying transport and the overflow-halving loop are the
// real code.

// A stalled embed endpoint is bounded by TimeoutMs on both the single and the
// batch path. Guards ollama_embedder.go embedOnce/embedBatchOnce:
// `httpClient := &http.Client{Timeout: timeout, …}`.
func TestFailureMode_OllamaEmbedder_StalledEndpointIsBoundedByTimeout(t *testing.T) {
	// The server only notices a client disconnect (and cancels r.Context())
	// once the request body has been consumed, so drain it before stalling;
	// `done` is a belt-and-braces release so srv.Close can never wait forever.
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done(): // the client gave up
		case <-done:
		}
	}))
	t.Cleanup(func() { close(done); srv.Close() })
	e := &OllamaEmbedder{BaseURL: srv.URL, Model: "m", TimeoutMs: 100}

	start := time.Now()
	if _, err := e.Embed(context.Background(), "hello"); err == nil {
		t.Fatal("Embed against a stalled endpoint must return an error")
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("Embed ignored timeout_ms=100: took %s", el)
	}

	start = time.Now()
	if _, err := e.EmbedBatch(context.Background(), []string{"a", "b"}); err == nil {
		t.Fatal("EmbedBatch against a stalled endpoint must return an error")
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("EmbedBatch ignored timeout_ms=100: took %s", el)
	}
}

// overflowEmbedServer rejects any input longer than window runes with Ollama's
// context-overflow 500 and records the rune length of every input it sees.
type overflowEmbedServer struct {
	mu      sync.Mutex
	window  int
	single  []int // rune length per /api/embeddings request
	batches []int // max rune length per /api/embed request
}

func (s *overflowEmbedServer) handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Retry-After", "0") // keep the transport's retry ladder from sleeping
	switch r.URL.Path {
	case "/api/embeddings":
		var req struct {
			Prompt string `json:"prompt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		n := len([]rune(req.Prompt))
		s.mu.Lock()
		s.single = append(s.single, n)
		s.mu.Unlock()
		if n > s.window {
			http.Error(w, `{"error":"the input length exceeds the context length"}`, http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embedding": []float64{0.1, 0.2}})
	case "/api/embed":
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		maxN := 0
		for _, in := range req.Input {
			if n := len([]rune(in)); n > maxN {
				maxN = n
			}
		}
		s.mu.Lock()
		s.batches = append(s.batches, maxN)
		s.mu.Unlock()
		if maxN > s.window {
			http.Error(w, `{"error":"the input length exceeds the context length"}`, http.StatusInternalServerError)
			return
		}
		out := make([][]float64, len(req.Input))
		for i := range out {
			out[i] = []float64{0.1, 0.2}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": out})
	default:
		http.NotFound(w, r)
	}
}

// distinct collapses consecutive repeats (the transport may replay the same
// size several times before the embedder halves it).
func distinct(sizes []int) []int {
	var out []int
	for _, n := range sizes {
		if len(out) == 0 || out[len(out)-1] != n {
			out = append(out, n)
		}
	}
	return out
}

// A context-overflow 500 halves the input and retries down to the model's
// window instead of failing the write. Guards ollama_embedder.go Embed:
// `if !overflow || len(runes) <= minEmbedRunes { return nil, err }; runes = runes[:len(runes)/2]`
// and the EmbedBatch `cap /= 2` loop.
func TestFailureMode_OllamaEmbedder_ContextOverflowHalvesAndRetries(t *testing.T) {
	s := &overflowEmbedServer{window: 600}
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	t.Cleanup(srv.Close)
	e := &OllamaEmbedder{BaseURL: srv.URL, Model: "m", TimeoutMs: 5000, MaxInputRunes: 2000}
	long := strings.Repeat("a", 3000)

	vec, err := e.Embed(context.Background(), long)
	if err != nil || len(vec) != 2 {
		t.Fatalf("Embed must recover from a context overflow by halving, got len=%d err=%v", len(vec), err)
	}
	s.mu.Lock()
	single := distinct(s.single)
	singleAttempts := len(s.single)
	s.mu.Unlock()
	want := []int{2000, 1000, 500}
	if len(single) != len(want) || single[0] != want[0] || single[1] != want[1] || single[2] != want[2] {
		t.Fatalf("Embed input sizes should halve 2000→1000→500, saw %v", single)
	}
	t.Logf("Embed: %d endpoint attempts across %d sizes (the shared transport replays an overflow 500 before the embedder halves it)", singleAttempts, len(single))

	vecs, err := e.EmbedBatch(context.Background(), []string{long, "short"})
	if err != nil || len(vecs) != 2 {
		t.Fatalf("EmbedBatch must recover from a context overflow by halving the cap, got n=%d err=%v", len(vecs), err)
	}
	s.mu.Lock()
	batches := distinct(s.batches)
	s.mu.Unlock()
	if len(batches) != len(want) || batches[0] != want[0] || batches[1] != want[1] || batches[2] != want[2] {
		t.Fatalf("EmbedBatch per-input cap should halve 2000→1000→500, saw %v", batches)
	}
}

// Below the floor the overflow is "some other problem" and must surface as an
// error rather than loop forever. Guards `len(runes) <= minEmbedRunes`.
func TestFailureMode_OllamaEmbedder_OverflowBelowFloorSurfaces(t *testing.T) {
	s := &overflowEmbedServer{window: 0} // everything overflows
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	t.Cleanup(srv.Close)
	e := &OllamaEmbedder{BaseURL: srv.URL, Model: "m", TimeoutMs: 5000, MaxInputRunes: 1024}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := e.Embed(ctx, strings.Repeat("a", 1024))
	if err == nil {
		t.Fatal("an overflow that persists below minEmbedRunes must return an error")
	}
	if ctx.Err() != nil {
		t.Fatalf("the halving loop did not terminate on its own: %v", err)
	}
	s.mu.Lock()
	sizes := distinct(s.single)
	s.mu.Unlock()
	// 1024 → 512 → 256 (floor, last attempt); never 128.
	if len(sizes) == 0 || sizes[len(sizes)-1] != 256 {
		t.Fatalf("halving must stop at the %d-rune floor, saw sizes %v", minEmbedRunes, sizes)
	}
}
