// Package memory — Late Chunker tests.
//
// The F1 acceptance bar is TestLateChunker_PoolingMath: for a
// known body + known chunk ranges, the mean-pool must match the
// manual mean of the corresponding per-token vectors. This is the
// regression bar for the late-chunking math — any change to
// lateChunker's mean-pool path must keep this test passing.
//
// Companion tests cover the fallback contract (D6):
//   - empty body → OptionC output, no embedder call
//   - over-cap body → OptionC output, no embedder call
//   - body without an embedder → OptionC output
package memory

import (
	"context"
	"testing"

	"github.com/cambrian-sh/core/domain"
)

// lateMockEmbedder satisfies PerTokenBatchEmbedder for tests. It
// returns the pre-set per-token vectors for the single text in the
// batch and tracks whether the embedder was invoked (so the
// fallback-path tests can assert the skip).
type lateMockEmbedder struct {
	perToken [][]float32
	called   bool
}

func (m *lateMockEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	_ = ctx
	_ = text
	return nil, nil
}

func (m *lateMockEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	_ = ctx
	_ = texts
	return nil, nil
}

func (m *lateMockEmbedder) EmbedBatchTokens(ctx context.Context, texts []string) ([][][]float32, error) {
	_ = ctx
	m.called = true
	return [][][]float32{m.perToken}, nil
}

func (m *lateMockEmbedder) isPerToken() {}

// TestLateChunker_PoolingMath (F1 regression bar, T-2.2).
// Body "aaaaaaaa\n\nbbbbbbbb" = 18 chars → 4 tokens at 4 chars/token.
// OptionC split: 2 paragraphs → 2 chunks.
//   chunk 0: "aaaaaaaa" (chars [0,8))  → tokens [0,1]
//   chunk 1: "bbbbbbbb" (chars [10,18)) → tokens [2,3]
//
// Per-token vectors:
//   token 0 = [1,1,1,1]
//   token 1 = [2,2,2,2]
//   token 2 = [3,3,3,3]
//   token 3 = [4,4,4,4]
//
// Mean-pool per chunk:
//   chunk 0 = mean([1,1,1,1], [2,2,2,2]) = [1.5, 1.5, 1.5, 1.5]
//   chunk 1 = mean([3,3,3,3], [4,4,4,4]) = [3.5, 3.5, 3.5, 3.5]
func TestLateChunker_PoolingMath(t *testing.T) {
	body := "aaaaaaaa\n\nbbbbbbbb"
	doc := &domain.ExternalDocument{
		SourceURI:  "test://pooling",
		SourceType: "file_drop",
		Title:      "t",
		Body:       body,
	}
	mock := &lateMockEmbedder{
		perToken: [][]float32{
			{1, 1, 1, 1},
			{2, 2, 2, 2},
			{3, 3, 3, 3},
			{4, 4, 4, 4},
		},
	}
	c := NewLateChunker(mock, 8192)
	chunks, err := c.Chunk(context.Background(), doc)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(chunks))
	}
	if !mock.called {
		t.Fatal("expected embedder to be called")
	}
	want0 := []float32{1.5, 1.5, 1.5, 1.5}
	want1 := []float32{3.5, 3.5, 3.5, 3.5}
	got0, ok := chunks[0].Metadata["late_embedding"].([]float32)
	if !ok {
		t.Fatalf("chunk 0 late_embedding not []float32: %T", chunks[0].Metadata["late_embedding"])
	}
	got1, ok := chunks[1].Metadata["late_embedding"].([]float32)
	if !ok {
		t.Fatalf("chunk 1 late_embedding not []float32: %T", chunks[1].Metadata["late_embedding"])
	}
	if !floatSlicesClose(got0, want0) {
		t.Errorf("chunk 0 mean-pool = %v, want %v", got0, want0)
	}
	if !floatSlicesClose(got1, want1) {
		t.Errorf("chunk 1 mean-pool = %v, want %v", got1, want1)
	}
}

// TestLateChunker_OverCapFallback (T-2.3).
// 500 chars / 4 chars-per-token = 125 tokens > 100 cap → embedder
// must NOT be called and chunks must be the OptionC output.
func TestLateChunker_OverCapFallback(t *testing.T) {
	body := "a"
	for i := 0; i < 500; i++ {
		body += "a"
	}
	doc := &domain.ExternalDocument{
		SourceURI:  "test://overcap",
		SourceType: "file_drop",
		Body:       body,
	}
	mock := &lateMockEmbedder{perToken: nil}
	c := NewLateChunker(mock, 100)
	chunks, err := c.Chunk(context.Background(), doc)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if mock.called {
		t.Fatal("over-cap body must not invoke the embedder")
	}
	if len(chunks) < 1 {
		t.Fatal("expected at least one chunk from the OptionC fallback")
	}
	for i, ch := range chunks {
		if _, hasLate := ch.Metadata["late_embedding"]; hasLate {
			t.Errorf("chunk %d unexpectedly carries late_embedding in over-cap fallback", i)
		}
	}
}

// TestLateChunker_EmptyBodyFallback.
// Empty body must skip the embedder and yield the OptionC empty
// chunk (one chunk with body "").
func TestLateChunker_EmptyBodyFallback(t *testing.T) {
	doc := &domain.ExternalDocument{
		SourceURI:  "test://empty",
		SourceType: "file_drop",
		Body:       "",
	}
	mock := &lateMockEmbedder{perToken: nil}
	c := NewLateChunker(mock, 8192)
	chunks, err := c.Chunk(context.Background(), doc)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if mock.called {
		t.Fatal("empty body must not invoke the embedder")
	}
	if len(chunks) != 1 {
		t.Fatalf("expected exactly 1 OptionC empty chunk, got %d", len(chunks))
	}
	if chunks[0].Body != "" {
		t.Errorf("expected empty body, got %q", chunks[0].Body)
	}
}

// TestLateChunker_NilEmbedder_FallsBackToOptionC.
// A LateChunker constructed without an embedder must not panic;
// the Chunk path falls back to OptionC output.
func TestLateChunker_NilEmbedder_FallsBackToOptionC(t *testing.T) {
	doc := &domain.ExternalDocument{
		SourceURI:  "test://nil",
		SourceType: "file_drop",
		Body:       "First paragraph.\n\nSecond paragraph.",
	}
	c := LateChunker{maxDocTokens: 8192}
	chunks, err := c.Chunk(context.Background(), doc)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("expected 2 OptionC chunks, got %d", len(chunks))
	}
	for i, ch := range chunks {
		if _, hasLate := ch.Metadata["late_embedding"]; hasLate {
			t.Errorf("chunk %d unexpectedly carries late_embedding without an embedder", i)
		}
	}
}

// TestLateChunker_DefaultMaxDocTokens.
// NewLateChunker with maxDocTokens <= 0 → 8192 default.
func TestLateChunker_DefaultMaxDocTokens(t *testing.T) {
	c := NewLateChunker(&lateMockEmbedder{}, 0)
	if c.maxDocTokens != 8192 {
		t.Errorf("maxDocTokens = %d, want 8192", c.maxDocTokens)
	}
}

// floatSlicesClose returns true if a and b have the same length and
// every element pair differs by less than 1e-5. The mean-pool
// divides by an integer count; the expected values are exact
// rationals that float32 represents exactly, so 1e-5 is a
// comfortable tolerance for the test.
func floatSlicesClose(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		diff := a[i] - b[i]
		if diff < 0 {
			diff = -diff
		}
		if diff > 1e-5 {
			return false
		}
	}
	return true
}
