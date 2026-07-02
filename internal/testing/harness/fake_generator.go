package harness

import (
	"context"
	"fmt"

	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// FakeGenerator returns scripted responses with configurable token counts.
type FakeGenerator struct {
	responses []string
	idx       int
}

func NewFakeGenerator(responses ...string) *FakeGenerator {
	return &FakeGenerator{responses: responses}
}

func (g *FakeGenerator) Generate(_ context.Context, prompt string) (string, error) {
	if g.idx >= len(g.responses) {
		panic(fmt.Sprintf("FakeGenerator: no response for prompt %q (index %d, total %d)", prompt, g.idx, len(g.responses)))
	}
	resp := g.responses[g.idx]
	g.idx++
	return resp, nil
}

func (g *FakeGenerator) Reset() {
	g.idx = 0
}

var _ domain.Generator = (*FakeGenerator)(nil)
