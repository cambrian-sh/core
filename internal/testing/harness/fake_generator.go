package harness

import (
	"context"
	"fmt"
	"sync"

	"github.com/cambrian-sh/core/domain"
)

// FakeGenerator returns scripted responses with configurable token counts.
//
// It is shared by concurrent ExecutePlan calls in the integration suite
// (TestE2E_ConwipBackpressure starts five goroutines on one harness), so the
// cursor is mutex-guarded. Before 2026-10-31 it was a bare int and the race
// went unnoticed only because `make integration` ran without -race.
type FakeGenerator struct {
	mu        sync.Mutex
	responses []string
	idx       int
}

func NewFakeGenerator(responses ...string) *FakeGenerator {
	return &FakeGenerator{responses: responses}
}

func (g *FakeGenerator) Generate(_ context.Context, prompt string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.idx >= len(g.responses) {
		panic(fmt.Sprintf("FakeGenerator: no response for prompt %q (index %d, total %d)", prompt, g.idx, len(g.responses)))
	}
	resp := g.responses[g.idx]
	g.idx++
	return resp, nil
}

func (g *FakeGenerator) Reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.idx = 0
}

var _ domain.Generator = (*FakeGenerator)(nil)
