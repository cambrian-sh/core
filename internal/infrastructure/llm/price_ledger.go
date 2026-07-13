package llm

import (
	"sync"

	"github.com/cambrian-sh/core/internal/config"
)

// PriceReader is the read-only port over the price ledger (ADR-0042 D6).
// Consumers (the EFE Costs map, metabolism accounting, a future cost optimizer)
// depend on this interface, not the concrete ledger, so the seam is stable when
// prices stop being static config and start being tracked dynamically.
type PriceReader interface {
	// Cost returns the per-1M-token input/output cost for a generator id.
	// ok is false for an unknown id.
	Cost(id string) (in, out float64, ok bool)
}

// PriceLedger is the single source of truth for per-id token cost. Config
// cost_per_1m_* values are a seed; the ledger is mutable so prices can later be
// updated at runtime without a config edit.
type PriceLedger struct {
	mu    sync.RWMutex
	costs map[string]priceEntry
}

type priceEntry struct {
	in  float64
	out float64
}

// NewPriceLedger returns an empty ledger.
func NewPriceLedger() *PriceLedger {
	return &PriceLedger{costs: make(map[string]priceEntry)}
}

// SeedPriceLedger builds a ledger from generator config (the declared seed).
func SeedPriceLedger(generators []config.GeneratorConfig) *PriceLedger {
	l := NewPriceLedger()
	for _, g := range generators {
		l.Set(g.ID, g.CostPer1MInput, g.CostPer1MOutput)
	}
	return l
}

// Cost implements PriceReader.
func (l *PriceLedger) Cost(id string) (in, out float64, ok bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	e, ok := l.costs[id]
	return e.in, e.out, ok
}

// Set overrides the cost for an id (seed update or runtime repricing).
func (l *PriceLedger) Set(id string, in, out float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.costs[id] = priceEntry{in: in, out: out}
}
