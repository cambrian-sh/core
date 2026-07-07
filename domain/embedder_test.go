package domain

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// mockEmbedder is a hand-rolled Embedder used to drive the forwarder tests
// below. It records every Embed call so the runtime tests can assert the
// call shape (order, count) at the call boundary, not just the return value.
type mockEmbedder struct {
	embed func(ctx context.Context, text string) ([]float32, error)

	embedCalls []string
}

func (m *mockEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	m.embedCalls = append(m.embedCalls, text)
	if m.embed == nil {
		return nil, errors.New("mockEmbedder: embed func not set")
	}
	return m.embed(ctx, text)
}

// Compile-time assertion: *mockEmbedder must satisfy the Embedder port. If
// the port signature ever drifts in a way the mock doesn't follow, this file
// stops compiling — the test cannot silently regress.
var _ Embedder = (*mockEmbedder)(nil)

// mockBatchEmbedder is a hand-rolled Embedder that ALSO implements
// EmbedBatch. It exists to verify the BatchEmbedder sub-interface compiles
// and to make the contract under test explicit.
type mockBatchEmbedder struct {
	mockEmbedder
	embedBatch func(ctx context.Context, texts []string) ([][]float32, error)

	batchCalls [][]string
}

func (m *mockBatchEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	m.batchCalls = append(m.batchCalls, texts)
	if m.embedBatch == nil {
		return nil, errors.New("mockBatchEmbedder: embedBatch func not set")
	}
	return m.embedBatch(ctx, texts)
}

// Compile-time assertion: *mockBatchEmbedder must satisfy the BatchEmbedder
// port (and therefore the Embedder port too, by transitivity).
var _ BatchEmbedder = (*mockBatchEmbedder)(nil)

// TestEmbedBatchForwarder is the happy path: a mock Embedder whose Embed
// returns a known vector per input; the forwarder returns the same vectors
// in the same order, and Embed is called exactly once per input in order.
func TestEmbedBatchForwarder(t *testing.T) {
	want := [][]float32{
		{0.1, 0.2, 0.3},
		{0.4, 0.5, 0.6},
		{0.7, 0.8, 0.9},
	}
	inputs := []string{"a", "b", "c"}
	m := &mockEmbedder{
		embed: func(ctx context.Context, text string) ([]float32, error) {
			switch text {
			case "a":
				return want[0], nil
			case "b":
				return want[1], nil
			case "c":
				return want[2], nil
			default:
				return nil, errors.New("unexpected text: " + text)
			}
		},
	}

	got, err := EmbedBatchForwarder(m, context.Background(), inputs)
	if err != nil {
		t.Fatalf("EmbedBatchForwarder: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if !equalVec(got[i], want[i]) {
			t.Errorf("got[%d] = %v, want %v", i, got[i], want[i])
		}
	}
	if len(m.embedCalls) != len(inputs) {
		t.Fatalf("Embed calls = %d, want %d", len(m.embedCalls), len(inputs))
	}
	for i, in := range inputs {
		if m.embedCalls[i] != in {
			t.Errorf("Embed call %d = %q, want %q", i, m.embedCalls[i], in)
		}
	}
}

// TestEmbedBatchForwarder_EmptyInput is the empty-input edge case: the
// forwarder returns [][]float32{} (non-nil, length 0) with no error, and
// Embed is not called at all.
func TestEmbedBatchForwarder_EmptyInput(t *testing.T) {
	m := &mockEmbedder{
		embed: func(ctx context.Context, text string) ([]float32, error) {
			return nil, errors.New("Embed should not be called for empty input")
		},
	}
	got, err := EmbedBatchForwarder(m, context.Background(), []string{})
	if err != nil {
		t.Fatalf("EmbedBatchForwarder(empty): %v", err)
	}
	if got == nil {
		t.Fatal("got = nil, want non-nil [][]float32{}")
	}
	if len(got) != 0 {
		t.Errorf("len(got) = %d, want 0", len(got))
	}
	if len(m.embedCalls) != 0 {
		t.Errorf("Embed calls = %d, want 0", len(m.embedCalls))
	}
}

// TestEmbedBatchForwarder_PropagatesError asserts that the forwarder wraps
// the underlying Embed error with the offending index, and that the original
// error is preserved (errors.Is) so callers can match on it.
func TestEmbedBatchForwarder_PropagatesError(t *testing.T) {
	sentinel := errors.New("backend timeout")
	m := &mockEmbedder{
		embed: func(ctx context.Context, text string) ([]float32, error) {
			if text == "b" {
				return nil, sentinel
			}
			return []float32{0.1, 0.2}, nil
		},
	}
	got, err := EmbedBatchForwarder(m, context.Background(), []string{"a", "b", "c"})
	if err == nil {
		t.Fatal("err = nil, want non-nil")
	}
	if got != nil {
		t.Errorf("got = %v, want nil on error", got)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("errors.Is(err, sentinel) = false, err = %v", err)
	}
	// The error message should mention the offending index so callers can
	// locate the failing input in the original slice.
	if msg := err.Error(); !strings.Contains(msg, "index 1") {
		t.Errorf("err.Error() = %q, want it to contain %q", msg, "index 1")
	}
	// The forwarder stops at the first failure: a, then b. c is never called.
	if len(m.embedCalls) != 2 {
		t.Errorf("Embed calls = %d, want 2 (a, b)", len(m.embedCalls))
	}
}

// TestEmbedBatchForwarder_NilInput covers the malformed-input edge case
// where texts is nil (distinct from empty). The contract matches the empty
// case: no error, no Embed call, non-nil empty result.
func TestEmbedBatchForwarder_NilInput(t *testing.T) {
	m := &mockEmbedder{
		embed: func(ctx context.Context, text string) ([]float32, error) {
			return nil, errors.New("Embed should not be called for nil input")
		},
	}
	got, err := EmbedBatchForwarder(m, context.Background(), nil)
	if err != nil {
		t.Fatalf("EmbedBatchForwarder(nil): %v", err)
	}
	if got == nil {
		t.Fatal("got = nil, want non-nil [][]float32{}")
	}
	if len(got) != 0 {
		t.Errorf("len(got) = %d, want 0", len(got))
	}
	if len(m.embedCalls) != 0 {
		t.Errorf("Embed calls = %d, want 0", len(m.embedCalls))
	}
}

// TestBatchEmbedderInterface_CompileTime surfaces the compile-time assertion
// to the test report so a reader can see the BatchEmbedder contract is being
// verified, not just the Embedder port.
func TestBatchEmbedderInterface_CompileTime(t *testing.T) {
	var b BatchEmbedder = &mockBatchEmbedder{
		mockEmbedder: mockEmbedder{
			embed: func(ctx context.Context, text string) ([]float32, error) {
				return []float32{1, 2, 3}, nil
			},
		},
		embedBatch: func(ctx context.Context, texts []string) ([][]float32, error) {
			out := make([][]float32, len(texts))
			for i := range texts {
				out[i] = []float32{float32(i), float32(i) + 0.5}
			}
			return out, nil
		},
	}
	// Embed and EmbedBatch are both reachable through the BatchEmbedder
	// interface — the sub-interface contract is "Embedder + EmbedBatch".
	if _, err := b.Embed(context.Background(), "x"); err != nil {
		t.Errorf("Embed: %v", err)
	}
	got, err := b.EmbedBatch(context.Background(), []string{"p", "q"})
	if err != nil {
		t.Errorf("EmbedBatch: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len(EmbedBatch result) = %d, want 2", len(got))
	}
}

func equalVec(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
