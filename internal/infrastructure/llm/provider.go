package llm

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cambrian-sh/core/domain"
	"github.com/cambrian-sh/core/internal/config"
)

// Provider is the concrete LLMProvider (ADR-0042). It composes the id-keyed
// registry, the circuit breaker (availability), the price ledger (cost), and the
// failover resolver into a single Acquire entry point. Preference is delegated:
// system roles read deterministic role config; agent steps consult the EFE
// preference hook (wired in 0042-08). The Provider only gates on health.
type Provider struct {
	// table holds the swappable generator state — registry, id order, capability
	// index and the global default — as ONE atomically-replaced value, so a live
	// reload (SaveGenerator/RemoveGenerator, owner directive 2026-08-12: "LLMs
	// register dynamically, not at start") can never expose a torn view: a
	// resolve either sees the whole old table or the whole new one. Reads stay
	// lock-free on the Acquire hot path.
	table   atomic.Pointer[generatorTable]
	breaker *CircuitBreaker
	ledger  *PriceLedger
	// roles maps role (Purpose) -> generator id. Guarded by rolesMu: the map is
	// read on every system-organ call and written by the operator plane's
	// SetRoleAssignment (contract 0096), which is what makes a role change take
	// effect on the next call rather than the next boot.
	roles   map[string]string
	rolesMu sync.RWMutex

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

// generatorTable is the swappable half of the Provider: everything derived from
// the generator LIST. Immutable once published — a reload builds a fresh one.
type generatorTable struct {
	registry  *GeneratorRegistry
	allIDs    []string
	capIndex  map[string][]string
	defaultID string
}

// tab returns the current table. Never nil after NewProvider.
func (p *Provider) tab() *generatorTable { return p.table.Load() }

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
	// Copied, not aliased: the map is mutated by SetRole under the Provider's
	// own lock, and mutating the caller's config struct through a shared map
	// would let a later config read observe a value no file ever stated.
	roles := make(map[string]string, len(cfg.Roles))
	for r, id := range cfg.Roles {
		roles[r] = id
		// A role naming an unknown generator is TOLERATED (the ladder falls back
		// to the default, ListRoleAssignments reports resolved=false) but never
		// silent: without this line the only symptom is the wrong model serving
		// an organ, which reads as a quality problem rather than a config one.
		if _, ok := reg.Lookup(id); !ok {
			log.Warn("llm provider: role names an unknown generator; the default will serve it",
				"role", r, "generator", id, "default", cfg.Default)
		}
	}
	p := &Provider{
		breaker: NewCircuitBreaker(cfg.Health.FailureThreshold, cooldown),
		ledger:  SeedPriceLedger(cfg.Generators),
		roles:   roles,
		sem:     sem,
		log:     log,
	}
	p.table.Store(&generatorTable{
		registry:  reg,
		allIDs:    reg.IDs(),
		capIndex:  reg.CapabilityIndex(),
		defaultID: cfg.Default,
	})
	return p, nil
}

// ReloadGenerators rebuilds the generator table from a new list and default,
// LIVE — the operator plane's SaveGenerator/RemoveGenerator apply here so a
// console-registered model is assignable and routable on the next call, no
// restart (the generator half of what contract 0096 did for roles). The NEW
// registry is built first: a bad spec fails the reload without touching the
// serving table. In-flight calls hold generators from the old table and finish
// on them — the same "nothing in flight moves" tolerance as SetRole. The
// breaker map is deliberately untouched: it is keyed by id with unknown-ids-
// healthy semantics, so a NEW id starts closed, and a REPLACED id keeps its
// history (its endpoint may be unchanged; one probe re-opens or clears it).
func (p *Provider) ReloadGenerators(gens []config.GeneratorConfig, defaultID string) error {
	reg, err := NewGeneratorRegistry(gens)
	if err != nil {
		return err
	}
	for _, g := range gens {
		p.ledger.Set(g.ID, g.CostPer1MInput, g.CostPer1MOutput)
	}
	p.table.Store(&generatorTable{
		registry:  reg,
		allIDs:    reg.IDs(),
		capIndex:  reg.CapabilityIndex(),
		defaultID: defaultID,
	})
	p.log.Info("llm provider: generator table reloaded live",
		"generators", len(gens), "default", defaultID)
	return nil
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
func (p *Provider) Registry() *GeneratorRegistry { return p.tab().registry }

// Default returns the global default generator id (interview-session base, etc.).
func (p *Provider) Default() string { return p.tab().defaultID }

// Acquire implements domain.LLMProvider: resolve a healthy model via the ladder,
// then return it wrapped in the health-recording decorator.
func (p *Provider) Acquire(ctx context.Context, req domain.LLMRequest) (domain.Generator, error) {
	id, err := p.resolve(ctx, req)
	if err != nil {
		p.log.Error("llm provider: no healthy model", "purpose", req.Purpose, "suggested", req.SuggestedModelID, "err", err)
		return nil, err
	}
	entry, ok := p.tab().registry.Lookup(id)
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

// GenerateWithTools forwards native tool-calling while holding a concurrency slot,
// mirroring Generate. See healthGenerator.GenerateWithTools on why forwarding is
// mandatory rather than optional.
// NativeToolsEnabled forwards the inner generator's capability report.
func (c *concurrencyGenerator) NativeToolsEnabled() bool {
	r, ok := c.inner.(domain.ToolCallingReporter)
	return !ok || r.NativeToolsEnabled()
}

func (c *concurrencyGenerator) GenerateWithTools(
	ctx context.Context, messages []domain.ModelMessage, tools []domain.ToolDefinition,
) (domain.ModelTurn, error) {
	tg, ok := c.inner.(domain.ToolCallingGenerator)
	if !ok {
		return domain.ModelTurn{}, fmt.Errorf(
			"llm provider: %T does not implement native tool-calling", c.inner)
	}
	if err := c.acquire(ctx); err != nil {
		return domain.ModelTurn{}, err
	}
	defer func() { <-c.sem }()
	return tg.GenerateWithTools(ctx, messages, tools)
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
	// One table load for the whole decision: allIDs/default/capIndex are always
	// from the SAME published table, even mid-reload.
	t := p.tab()
	return resolveModel(
		req.SuggestedModelID,
		req.CapabilityHints,
		p.preferenceFor(ctx, req),
		t.allIDs,
		t.defaultID,
		p.breaker.Healthy,
		t.capIndex,
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
	p.rolesMu.RLock()
	id, ok := p.roles[string(req.Purpose)]
	p.rolesMu.RUnlock()
	if ok {
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

// GeneratorForModel returns a Generator that PREFERS the named generator id on
// every call, falling down the ordinary failover ladder when it is unhealthy
// or unknown (ADR-0112 §15: the Ingress Studio's drafting model is operator
// configuration, resolved per call so a change needs no restart).
func (p *Provider) GeneratorForModel(id string) domain.Generator {
	return &modelGenerator{provider: p, id: id}
}

type modelGenerator struct {
	provider *Provider
	id       string
}

func (g *modelGenerator) Generate(ctx context.Context, prompt string) (string, error) {
	gen, err := g.provider.Acquire(ctx, domain.LLMRequest{SuggestedModelID: g.id})
	if err != nil {
		return "", err
	}
	return gen.Generate(ctx, prompt)
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

// GenerateWithTools acquires a generator for this purpose and forwards native
// tool-calling. The acquired generator decides the capability, so this returns an
// error rather than silently degrading when the resolved generator cannot do it.
func (g *purposeGenerator) GenerateWithTools(
	ctx context.Context, messages []domain.ModelMessage, tools []domain.ToolDefinition,
) (domain.ModelTurn, error) {
	gen, err := g.provider.Acquire(ctx, domain.LLMRequest{Purpose: g.purpose, CapabilityHints: g.hints})
	if err != nil {
		return domain.ModelTurn{}, err
	}
	tg, ok := gen.(domain.ToolCallingGenerator)
	if !ok {
		return domain.ModelTurn{}, fmt.Errorf(
			"llm provider: acquired %T does not implement native tool-calling", gen)
	}
	return tg.GenerateWithTools(ctx, messages, tools)
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
	_ domain.LLMProvider = (*Provider)(nil)
	_ domain.Generator   = (*purposeGenerator)(nil)
	_ interface {
		GenerateStream(ctx context.Context, prompt string) (<-chan domain.StreamChunk, error)
	} = (*purposeGenerator)(nil)
)

// BreakerState reports the circuit breaker's view of one generator, as
// "closed" (healthy and taking traffic) or "open" (shedding after consecutive
// failures). Added for the operator plane's ListGenerators (contract 0072).
//
// The breaker is PASSIVE — it learns health only from traffic that actually
// flowed (ADR-0042 D4) — so "closed" on an idle generator means "nothing has
// failed", not "verified working". TestGenerator is the active check.
func (p *Provider) BreakerState(id string) string {
	if p.breaker == nil {
		return "closed"
	}
	if p.breaker.Healthy(id) {
		return "closed"
	}
	return "open"
}

// Roles returns the role → generator-id map (planner/verifier/router/interview/
// memory). Copied, so a caller cannot mutate the Provider's routing table.
func (p *Provider) Roles() map[string]string {
	p.rolesMu.RLock()
	defer p.rolesMu.RUnlock()
	out := make(map[string]string, len(p.roles))
	maps.Copy(out, p.roles)
	return out
}

// SetRole rebinds one system role to a generator id, effective on the NEXT call
// that organ makes — resolution reads this map per call, so nothing in flight
// moves. This is the live half of the operator plane's SetRoleAssignment
// (contract 0096); durability is the store's job, not this method's.
//
// Validation (does the id name a real generator?) belongs to the write path,
// which can refuse; here an unknown id would behave exactly like one configured
// in a file — the failover ladder falls back to the default.
func (p *Provider) SetRole(role, generatorID string) {
	p.rolesMu.Lock()
	defer p.rolesMu.Unlock()
	p.roles[role] = generatorID
}

// KnowsGenerator reports whether id names a registered generator. It backs
// RoleAssignmentOp.resolved: a role pointing at a removed generator silently
// falls back to the default, and nothing else in the system says so.
func (p *Provider) KnowsGenerator(id string) bool {
	reg := p.tab().registry
	if reg == nil {
		return false
	}
	_, ok := reg.Lookup(id)
	return ok
}
