// generatorRuntime is the LIVE generator list + default — boot config plus
// every SaveGenerator/RemoveGenerator since (the generator analogue of
// mcpRuntime, contract 0097's pattern). The operator plane's read half and the
// write path's guards consult THIS, not the boot snapshot, so a console
// re-read after a save shows the generator it just made, TestGenerator can
// probe it, and the remove/default guards judge current state rather than
// boot state.
package app

import (
	"sync"

	"github.com/cambrian-sh/core/internal/config"
)

type generatorRuntime struct {
	mu        sync.RWMutex
	gens      []config.GeneratorConfig
	defaultID string
}

func newGeneratorRuntime(cfg config.LLMProviderConfig) *generatorRuntime {
	g := &generatorRuntime{defaultID: cfg.Default}
	g.gens = append(g.gens, cfg.Generators...)
	return g
}

// list returns a copy — callers must not be able to mutate the shared slice.
func (g *generatorRuntime) list() []config.GeneratorConfig {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]config.GeneratorConfig, len(g.gens))
	copy(out, g.gens)
	return out
}

func (g *generatorRuntime) defaultGenerator() string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.defaultID
}

// set replaces the list and default together — one lock, no torn read.
func (g *generatorRuntime) set(gens []config.GeneratorConfig, defaultID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gens = make([]config.GeneratorConfig, len(gens))
	copy(g.gens, gens)
	g.defaultID = defaultID
}
