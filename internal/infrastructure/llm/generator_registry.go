package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// GeneratorEntry is a constructed generator plus the metadata the Provider needs
// to route to and account for it. Keyed by the stable generator id (ADR-0042 D2).
type GeneratorEntry struct {
	ID           string
	Generator    domain.Generator
	Provider     string
	Capabilities []string
}

// GeneratorRegistry holds one client per generator id — replacing the legacy
// one-slot-per-provider ProviderRegistry, so N OpenAI-compatible models coexist.
type GeneratorRegistry struct {
	entries map[string]GeneratorEntry
	order   []string // preserves config order for stable iteration
}

// NewGeneratorRegistry constructs a client for every generator via NewClient (the
// one constructor), keyed by id.
func NewGeneratorRegistry(generators []config.GeneratorConfig) (*GeneratorRegistry, error) {
	r := &GeneratorRegistry{entries: make(map[string]GeneratorEntry, len(generators))}
	for _, g := range generators {
		gen, err := NewClient(g)
		if err != nil {
			return nil, fmt.Errorf("generator %q: %w", g.ID, err)
		}
		r.entries[g.ID] = GeneratorEntry{
			ID:           g.ID,
			Generator:    gen,
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
	if !isRequestFault(err) {
		h.breaker.Record(ctx, h.id, err == nil && strings.TrimSpace(out) != "")
	}
	return out, err
}

// NativeToolsEnabled forwards the inner generator's capability report. Forwarding
// GenerateWithTools without this would make the wrapper claim tool support on behalf
// of an inner that has it disabled — over-reporting is as wrong as under-reporting,
// just in the other direction. domain.SupportsToolCalling also answers false for an
// inner with no GenerateWithTools at all, which a bare reporter assertion would read
// as capable (T-20261105-core-health-and-spend-meter-decorators-report).
func (h *healthGenerator) NativeToolsEnabled() bool {
	return domain.SupportsToolCalling(h.inner)
}

// GenerateWithTools forwards native tool-calling through the health wrapper and
// records the outcome on the breaker, exactly as Generate does.
//
// A decorator that forwards Generate but NOT this silently downgrades a capable
// generator to the text path — the capability simply disappears, with no error and no
// log. That is the same silent-wiring shape as the EFE task rebuild dropping
// RequiredCapabilities and base.think() shadowing the tool-round default; both cost a
// day. Hence a forwarding test per decorator. ADR-0097 D1.
func (h *healthGenerator) GenerateWithTools(
	ctx context.Context, messages []domain.ModelMessage, tools []domain.ToolDefinition,
) (domain.ModelTurn, error) {
	tg, ok := h.inner.(domain.ToolCallingGenerator)
	if !ok {
		return domain.ModelTurn{}, fmt.Errorf(
			"llm provider: health-wrapped %T does not implement native tool-calling", h.inner)
	}
	out, err := tg.GenerateWithTools(ctx, messages, tools)
	// Same health signal as Generate: a turn that produced neither text nor a tool
	// call is a failed call, even without an error.
	if !isRequestFault(err) {
		h.breaker.Record(ctx, h.id, err == nil && (strings.TrimSpace(out.Text) != "" || len(out.ToolCalls) > 0))
	}
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
		h.breaker.Record(ctx, h.id, false)
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
		h.breaker.Record(ctx, h.id, hadContent)
	}()
	return out, nil
}

// GenerateRequest forwards the C1 request path and records the outcome on the
// breaker exactly as Generate does. A domain.ErrUnsupported refusal is the
// REQUEST's fault, not the endpoint's, so it is not a health failure.
func (h *healthGenerator) GenerateRequest(ctx context.Context, req domain.GenerateRequest) (domain.GenerateResponse, error) {
	rg, ok := domain.AsRequestGenerator(h.inner)
	if !ok {
		return domain.GenerateResponse{}, fmt.Errorf("llm provider: health-wrapped %T does not implement GenerateRequest", h.inner)
	}
	out, err := rg.GenerateRequest(ctx, req)
	if !isRequestFault(err) {
		h.breaker.Record(ctx, h.id, err == nil && (strings.TrimSpace(out.Text) != "" || len(out.ToolCalls) > 0))
	}
	return out, err
}

// StreamRequest forwards the C1 stream and records its outcome when it ends.
func (h *healthGenerator) StreamRequest(ctx context.Context, req domain.GenerateRequest) (<-chan domain.StreamChunk, error) {
	rg, ok := domain.AsRequestGenerator(h.inner)
	if !ok {
		return nil, fmt.Errorf("llm provider: health-wrapped %T does not implement StreamRequest", h.inner)
	}
	in, err := rg.StreamRequest(ctx, req)
	if err != nil {
		if !isRequestFault(err) {
			h.breaker.Record(ctx, h.id, false)
		}
		return nil, err
	}
	out := make(chan domain.StreamChunk, 64)
	go func() {
		defer close(out)
		var hadContent, fault bool
		for c := range in {
			if c.Err != nil {
				fault = isRequestFault(c.Err)
			} else if c.Text != "" || (c.Response != nil && len(c.Response.ToolCalls) > 0) {
				hadContent = true
			}
			out <- c
		}
		if !fault {
			h.breaker.Record(ctx, h.id, hadContent)
		}
	}()
	return out, nil
}

// Capabilities forwards the inner generator's declaration.
func (h *healthGenerator) Capabilities() domain.Capabilities { return capabilitiesOf(h.inner) }

// RequestsEnabled forwards whether the inner is a RequestGenerator at all.
func (h *healthGenerator) RequestsEnabled() bool {
	_, ok := domain.AsRequestGenerator(h.inner)
	return ok
}

// capabilitiesOf returns v's declared capabilities, or the zero value (everything
// unsupported) when v is not a RequestGenerator.
func capabilitiesOf(v any) domain.Capabilities {
	if rg, ok := domain.AsRequestGenerator(v); ok {
		return rg.Capabilities()
	}
	return domain.Capabilities{}
}

// isRequestFault reports a failure that is the REQUEST's, not the endpoint's: a
// field the model cannot honour (domain.ErrUnsupported) or a prompt over its context
// window (domain.ErrContextOverflow). Neither is a health event — recording it would
// open the breaker on a healthy model because one caller sent too much
// (T-20261112-worker-resolve-cid-inline-has-no-size-cap-a).
func isRequestFault(err error) bool {
	return errors.Is(err, domain.ErrUnsupported) || errors.Is(err, domain.ErrContextOverflow)
}
