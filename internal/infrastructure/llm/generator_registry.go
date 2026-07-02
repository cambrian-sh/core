package llm

import (
	"context"
	"fmt"
	"strings"

	"github.com/cambrian-sh/cambrian-runtime/internal/config"
	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// GeneratorEntry is a constructed generator plus the metadata the Provider needs
// to route to and account for it. Keyed by the stable generator id (ADR-0042 D2).
type GeneratorEntry struct {
	ID           string
	Generator    domain.Generator
	Extractor    TokenUsageExtractor
	Provider     string
	Capabilities []string
}

// GeneratorRegistry holds one client per generator id — replacing the legacy
// one-slot-per-provider ProviderRegistry, so N OpenAI-compatible models coexist.
type GeneratorRegistry struct {
	entries map[string]GeneratorEntry
	order   []string // preserves config order for stable iteration
}

// NewGeneratorRegistry constructs a client for every generator via the existing
// provider factory, keyed by id.
func NewGeneratorRegistry(generators []config.GeneratorConfig) (*GeneratorRegistry, error) {
	r := &GeneratorRegistry{entries: make(map[string]GeneratorEntry, len(generators))}
	for _, g := range generators {
		gen, ext, err := NewClient(config.ModelConfig{
			Provider:        g.Provider,
			Model:           g.Model,
			Endpoint:        g.Endpoint,
			APIKeyEnv:       g.APIKeyEnv,
			CostPer1MInput:  g.CostPer1MInput,
			CostPer1MOutput: g.CostPer1MOutput,
			TimeoutMs:       g.TimeoutMs,
			Capabilities:    g.Capabilities,
		})
		if err != nil {
			return nil, fmt.Errorf("generator %q: %w", g.ID, err)
		}
		r.entries[g.ID] = GeneratorEntry{
			ID:           g.ID,
			Generator:    gen,
			Extractor:    ext,
			Provider:     g.Provider,
			Capabilities: g.Capabilities,
		}
		r.order = append(r.order, g.ID)
	}
	return r, nil
}

// Lookup returns the entry for an id.
func (r *GeneratorRegistry) Lookup(id string) (GeneratorEntry, bool) {
	e, ok := r.entries[id]
	return e, ok
}

// IDs returns generator ids in config order.
func (r *GeneratorRegistry) IDs() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// CapabilityIndex maps each capability to the ids advertising it (config order).
func (r *GeneratorRegistry) CapabilityIndex() map[string][]string {
	idx := make(map[string][]string)
	for _, id := range r.order {
		for _, cap := range r.entries[id].Capabilities {
			idx[cap] = append(idx[cap], id)
		}
	}
	return idx
}

// healthGenerator wraps a Generator so every Generate call feeds its outcome
// into the circuit breaker (ADR-0042 D3/D4). Organs stay blind: they hold a
// plain domain.Generator and never see the id or the breaker. An empty response
// counts as a failure — the generalized fix for the swallowed-empty-response bug.
type healthGenerator struct {
	id      string
	inner   domain.Generator
	breaker *CircuitBreaker
}

// newHealthGenerator wraps inner; if breaker is nil the wrapper is a pass-through.
func newHealthGenerator(id string, inner domain.Generator, breaker *CircuitBreaker) domain.Generator {
	if breaker == nil {
		return inner
	}
	return &healthGenerator{id: id, inner: inner, breaker: breaker}
}

func (h *healthGenerator) Generate(ctx context.Context, prompt string) (string, error) {
	out, err := h.inner.Generate(ctx, prompt)
	ok := err == nil && strings.TrimSpace(out) != ""
	h.breaker.Record(h.id, ok)
	return out, err
}

// streamingInner is the optional streaming capability a Generator may
// implement. The health wrapper delegates to its inner when present so the
// circuit breaker still records the call's outcome.
type streamingInner interface {
	GenerateStream(ctx context.Context, prompt string) (<-chan domain.StreamChunk, error)
}

// GenerateStream delegates to the inner generator when it supports streaming;
// records the final outcome (ok = non-empty body, no error) to the breaker.
// Returns a non-nil error if the inner does not implement streaming — the
// caller (purposeGenerator.GenerateStream, or the EdgeExtractor's fallback)
// decides whether to fall back to Generate.
func (h *healthGenerator) GenerateStream(ctx context.Context, prompt string) (<-chan domain.StreamChunk, error) {
	sg, ok := h.inner.(streamingInner)
	if !ok {
		return nil, fmt.Errorf("llm provider: health-wrapped %T does not implement streaming", h.inner)
	}
	in, err := sg.GenerateStream(ctx, prompt)
	if err != nil {
		h.breaker.Record(h.id, false)
		return nil, err
	}
	// Wrap the stream so the breaker sees the final outcome (empty body or
	// stream_error → failure; non-empty body → success). The forwarded
	// channel preserves ordering and the IsFinal marker.
	out := make(chan domain.StreamChunk, 64)
	go func() {
		defer close(out)
		var hadContent bool
		for c := range in {
			if c.Text != "" {
				hadContent = true
			}
			out <- c
			if c.IsFinal {
				break
			}
		}
		h.breaker.Record(h.id, hadContent)
	}()
	return out, nil
}
