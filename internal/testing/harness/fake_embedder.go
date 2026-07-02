package harness

import (
	"context"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// FakeEmbedder returns fixed similarity vectors for testing.
type FakeEmbedder struct {
	vecs map[string][]float32
}

func NewFakeEmbedder() *FakeEmbedder {
	return &FakeEmbedder{vecs: make(map[string][]float32)}
}

func (e *FakeEmbedder) Set(text string, vec []float32) {
	e.vecs[text] = vec
}

func (e *FakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if v, ok := e.vecs[text]; ok {
		return v, nil
	}
	return make([]float32, 1536), nil
}

var _ domain.Embedder = (*FakeEmbedder)(nil)
