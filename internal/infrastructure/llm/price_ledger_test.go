package llm

import (
	"testing"

	"github.com/cambrian-sh/core/internal/config"
)

func TestPriceLedger_SeedAndRead(t *testing.T) {
	l := SeedPriceLedger([]config.GeneratorConfig{
		{ID: "qwen-local", CostPer1MInput: 0, CostPer1MOutput: 0},
		{ID: "deepseek", CostPer1MInput: 0.0015, CostPer1MOutput: 0.002},
	})

	in, out, ok := l.Cost("deepseek")
	if !ok || in != 0.0015 || out != 0.002 {
		t.Fatalf("deepseek cost: got (%v,%v,%v)", in, out, ok)
	}
	// default-generator id resolves to its seeded (zero) cost, still ok=true.
	if in, out, ok := l.Cost("qwen-local"); !ok || in != 0 || out != 0 {
		t.Fatalf("qwen-local cost: got (%v,%v,%v)", in, out, ok)
	}
}

func TestPriceLedger_UnknownIDSafe(t *testing.T) {
	l := NewPriceLedger()
	if in, out, ok := l.Cost("ghost"); ok || in != 0 || out != 0 {
		t.Fatalf("unknown id should be (0,0,false), got (%v,%v,%v)", in, out, ok)
	}
}

func TestPriceLedger_SetOverridesSeed(t *testing.T) {
	l := SeedPriceLedger([]config.GeneratorConfig{{ID: "deepseek", CostPer1MInput: 0.0015, CostPer1MOutput: 0.002}})
	l.Set("deepseek", 0.005, 0.010) // runtime repricing
	if in, out, _ := l.Cost("deepseek"); in != 0.005 || out != 0.010 {
		t.Fatalf("Set should override seed, got (%v,%v)", in, out)
	}
}

// Ensure the concrete type satisfies the read-only port.
var _ PriceReader = (*PriceLedger)(nil)
