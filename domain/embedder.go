package domain

import "context"

// Embedder converts text to a float32 embedding vector.
// Consumers that also need batch embedding should define their own local
// batchEmbedder interface that embeds this one.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}
