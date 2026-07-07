// This file declares the Embedder port (the single-text vectorizer) and the
// optional BatchEmbedder sub-interface (an efficiency-oriented capability
// for backends that can vectorize a batch of texts in a single call, e.g.
// Ollama's /api/embeddings with `input: [...]`). The default forwarder
// EmbedBatchForwarder loops over Embed for plain Embedder values; callers
// that don't know whether an implementation is batch-capable should
// type-assert to BatchEmbedder and fall back to the forwarder.
// See ADR-0060 D3.
package domain

import (
	"context"
	"fmt"
)

// Embedder converts text to a float32 embedding vector.
// Consumers that also need batch embedding should define their own local
// batchEmbedder interface that embeds this one.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// BatchEmbedder is an optional capability on top of Embedder. Implementations
// that can vectorize a batch of texts in a single backend call (e.g. Ollama's
// /api/embeddings with `input: [...]`) override EmbedBatch for efficiency.
// Callers must use the EmbedBatchForwarder helper when they don't know
// whether the implementation is batch-capable.
type BatchEmbedder interface {
	Embedder
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
}

// EmbedBatchForwarder calls e.Embed for each text in texts, in order, and
// returns the vectors in the same order. This is the default loop-based
// implementation of the BatchEmbedder contract for plain Embedder values.
// The default forwarder returns [][]float32{} for an empty input (no error).
func EmbedBatchForwarder(e Embedder, ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return [][]float32{}, nil
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, err := e.Embed(ctx, t)
		if err != nil {
			return nil, fmt.Errorf("embed batch forwarder at index %d: %w", i, err)
		}
		out[i] = v
	}
	return out, nil
}
