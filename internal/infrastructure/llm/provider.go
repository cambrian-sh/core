package llm

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/cambrian-sh/core/internal/config"
	"github.com/cambrian-sh/core/domain"
)

// Provider is the concrete LLMProvider (ADR-0042). It composes the id-keyed
// registry, the circuit breaker (availability), the price ledger (cost), and the
// failover resolver into a single Acquire entry point. Preference is delegated:
// system roles read deterministic role config; agent steps consult the EFE
// preference hook (wired in 0042-08). The Provider only gates on health.
type Provider struct {
	registry  *GeneratorRegistry
	breaker   *CircuitBreaker
	ledger    *PriceLedger
	roles     map[string]string // role (Purpose) -> generator id
	defaultID string
	allIDs    []string
	capIndex  map[string][]string

	// agentStepPreference supplies the ordered EFE/auction candidate ids for an
	// agent step. Nil until ADR-0037 is wired (0042-08); a nil hook means the
	// ladder relies on suggestion -> default -> capability match.
	agentStepPreference func(ctx context.Context, req domain.LLMRequest) []string

	// traceWrapper decorates every acquired generator with cross-cutting
	// observability (Langfuse), labelled by purpose. Injected from main so the
	// Provider stays decoupled from the premium layer. Nil = no tracing. Because
	// every LLM call flows through Acquire, wrapping here traces them all — no
	// per-call-site wrapping to forget (ADR-0042 + ADR-0019).
	traceWrapper func(gen domain.Generator, subsystem string) domain.Generator

	// sem bounds the number of concurrent in-flight LLM calls across EVERY call path
	// (agents + planner + verifier + agentic-retrieval + consolidator). Nil ⇒ no cap.
	// It composes with the LLMGateway CONWIP semaphore (which only gates agent calls);
	// this is the global backstop that stops direct system-organ calls from flooding a
	// rate-limited provider (HTTP 429). ADR-0042 chokepoint.
	sem chan struct{}

	log *slog.Logger
}

// defaultLLMMaxConcurrency bounds total in-flight LLM calls when the config leaves
// max_concurrency at 0. Chosen to stay under typical hosted-endpoint rate limits while
// preserving useful parallelism for the fan-out (planner + agents + agentic sub-queries).
const defaultLLMMaxConcurrency = 8

// NewProvider builds the Provider from the llm_provider config block.
func NewProvider(cfg config.LLMProviderConfig, log *slog.Logger) (*Provider, error) {
	reg, err := NewGeneratorRegistry(cfg.Generators)
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	cooldown := time.Duration(cfg.Health.CooldownMs) * time.Millisecond
	// Global concurrency cap: 0 ⇒ default, negative ⇒ disabled (unbounded).
	maxConc := cfg.MaxConcurrency
	if maxConc == 0 {
		maxConc = defaultLLMMaxConcurrency
	}
	var sem chan struct{}
	if maxConc > 0 {
		sem = make(chan struct{}, maxConc)
	}
	return &Provider{
		registry:  reg,
		breaker:   NewCircuitBreaker(cfg.Health.FailureThreshold, cooldown),
		ledger:    SeedPriceLedger(cfg.Generators),
		roles:     cfg.Roles,
		defaultID: cfg.Default,
		allIDs:    reg.IDs(),
		capIndex:  reg.CapabilityIndex(),
		sem:       sem,
		log:       log,
	}, nil
}

// SetAgentStepPreference injects the EFE/auction preference source for agent
// steps (ADR-0037, wired in 0042-08).
func (p *Provider) SetAgentStepPreference(fn func(ctx context.Context, req domain.LLMRequest) []string) {
	p.agentStepPreference = fn
}

// SetTraceWrapper injects the observability decorator applied to every acquired
// generator (e.g. Langfuse). Must be set during bootstrap, before serving.
func (p *Provider) SetTraceWrapper(fn func(gen domain.Generator, subsystem string) domain.Generator) {
	p.traceWrapper = fn
}

// SetHealthEventBus wires the EventBus the circuit breaker publishes
// LLMHealthEvents to on an open↔closed transition (ADR-0047 D3). Bootstrap-time.
func (p *Provider) SetHealthEventBus(bus domain.EventBus) {
	if p.breaker != nil {
		p.breaker.Bus = bus
	}
}

// Ledger exposes the price ledger (read/write) for cost wiring.
func (p *Provider) Ledger() *PriceLedger { return p.ledger }

// Registry exposes the generator registry for auction agent registration.
func (p *Provider) Registry() *GeneratorRegistry { return p.registry }

// Default returns the global default generator id (interview-session base, etc.).
func (p *Provider) Default() string { return p.defaultID }

// Acquire implements domain.LLMProvider: resolve a healthy model via the ladder,
// then return it wrapped in the health-recording decorator.
func (p *Provider) Acquire(ctx context.Context, req domain.LLMRequest) (domain.Generator, error) {
	id, err := p.resolve(ctx, req)
	if err != nil {
		p.log.Error("llm provider: no healthy model", "purpose", req.Purpose, "suggested", req.SuggestedModelID, "err", err)
		return nil, err
	}
	entry, ok := p.registry.Lookup(id)
	if !ok {
		return nil, fmt.Errorf("llm provider: resolved id %q not in registry", id)
	}
	// Health-recording (inner) so the breaker sees outcomes; tracing (outer) so
	// every call is observed by purpose. Tracing is a no-op when unset.
	gen := newHealthGenerator(id, entry.Generator, p.breaker)
	if p.traceWrapper != nil {
		gen = p.traceWrapper(gen, string(req.Purpose))
	}
	// Outermost: the global concurrency cap gates the entire (traced, health-recorded)
	// call, so a burst of direct system-organ calls cannot flood the provider.
	if p.sem != nil {
		gen = &concurrencyGenerator{inner: gen, sem: p.sem}
	}
	return gen, nil
}

// concurrencyGenerator bounds concurrent in-flight LLM calls via a shared semaphore.
// Generate holds a slot for the call; GenerateStream holds a slot until the returned
// channel closes (the whole stream). Acquisition respects the request's context deadline,
// so a saturated cap surfaces as a context error, not a hang.
type concurrencyGenerator struct {
	inner domain.Generator
	sem   chan struct{}
}

func (c *concurrencyGenerator) acquire(ctx context.Context) error {
	select {
	case c.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *concurrencyGenerator) Generate(ctx context.Context, prompt string) (string, error) {
	if err := c.acquire(ctx); err != nil {
		return "", err
	}
	defer func() { <-c.sem }()
	return c.inner.Generate(ctx, prompt)
}

func (c *concurrencyGenerator) GenerateStream(ctx context.Context, prompt string) (<-chan domain.StreamChunk, error) {
	sg, ok := c.inner.(streamingInner)
	if !ok {
		return nil, fmt.Errorf("llm provider: concurrency-wrapped %T does not implement streaming", c.inner)
	}
	if err := c.acquire(ctx); err != nil {
		return nil, err
	}
	in, err := sg.GenerateStream(ctx, prompt)
	if err != nil {
		<-c.sem
		return nil, err
	}
	out := make(chan domain.StreamChunk, 64)
	go func() {
		defer func() { <-c.sem }() // release only when the whole stream is drained
		defer close(out)
		for chunk := range in {
			out <- chunk
		}
	}()
	return out, nil
}

// resolve picks the generator id via the failover ladder, sourcing preference by
// purpose. Separated from Acquire so the decision is unit-testable.
func (p *Provider) resolve(ctx context.Context, req domain.LLMRequest) (string, error) {
	return resolveModel(
		req.SuggestedModelID,
		req.CapabilityHints,
		p.preferenceFor(ctx, req),
		p.allIDs,
		p.defaultID,
		p.breaker.Healthy,
		p.capIndex,
	)
}

// preferenceFor returns the ordered preference ids for a request. The dispatch
// chooses the preference *source* (config vs EFE) — it does not hardcode which
// model serves a task, so the Zero-Hardcode Rule holds.
func (p *Provider) preferenceFor(ctx context.Context, req domain.LLMRequest) []string {
	if req.Purpose == domain.PurposeAgentStep {
		if p.agentStepPreference != nil {
			return p.agentStepPreference(ctx, req)
		}
		return nil
	}
	// System role: deterministic role -> id (Zero-Hardcode-legal; roles are not agents).
	if id, ok := p.roles[string(req.Purpose)]; ok {
		return []string{id}
	}
	return nil
}

// GeneratorFor returns a domain.Generator bound to a fixed purpose that resolves
// a healthy model via Acquire on every Generate call — giving live per-call
// failover. This is what system organs are injected with (ADR-0042 D5).
func (p *Provider) GeneratorFor(purpose domain.Purpose, hints ...string) domain.Generator {
	return &purposeGenerator{provider: p, purpose: purpose, hints: hints}
}

type purposeGenerator struct {
	provider *Provider
	purpose  domain.Purpose
	hints    []string
}

func (g *purposeGenerator) Generate(ctx context.Context, prompt string) (string, error) {
	gen, err := g.provider.Acquire(ctx, domain.LLMRequest{Purpose: g.purpose, CapabilityHints: g.hints})
	if err != nil {
		return "", err
	}
	return gen.Generate(ctx, prompt)
}

// GenerateStream acquires a healthy generator and delegates to its streaming
// surface, if any. Returns nil + a non-nil error if the inner generator
// does not implement streaming; callers can fall back to Generate. ADR-0042
// D5 live-failover applies identically to streaming calls.
func (g *purposeGenerator) GenerateStream(ctx context.Context, prompt string) (<-chan domain.StreamChunk, error) {
	gen, err := g.provider.Acquire(ctx, domain.LLMRequest{Purpose: g.purpose, CapabilityHints: g.hints})
	if err != nil {
		return nil, err
	}
	sg, ok := gen.(interface {
		GenerateStream(ctx context.Context, prompt string) (<-chan domain.StreamChunk, error)
	})
	if !ok {
		return nil, fmt.Errorf("llm provider: generator %T does not implement streaming", gen)
	}
	return sg.GenerateStream(ctx, prompt)
}

var (
	_ domain.LLMProvider                       = (*Provider)(nil)
	_ domain.Generator                         = (*purposeGenerator)(nil)
	_ interface {
		GenerateStream(ctx context.Context, prompt string) (<-chan domain.StreamChunk, error)
	} = (*purposeGenerator)(nil)
)
