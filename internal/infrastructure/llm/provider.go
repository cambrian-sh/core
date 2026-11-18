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

// RetiredRoles are llm_provider.roles keys whose organ no longer exists, with the
// reason. A config (file, store or env) that still names one loads unchanged; the
// Provider drops the binding at construction with a log line.
var RetiredRoles = map[string]string{
	"interview": "the LLM agent interview was deleted 2026-11-05 (T-20261105-delete-the-llm-agent-interview-now-owner)",
}

// RoleFallbacks maps a purpose to the role it resolves through when no role of
// its own is configured in llm_provider.roles. It is the documented alias table
// that keeps a newly split purpose from changing routing: `thought` (in-kernel
// thought steps, split off `planner` so hooks and spend can tell a deliverable
// from a plan) is served by the planner's generator until an operator binds a
// `thought` role. One hop only; the table is config vocabulary, not routing —
// the role values themselves come from config (Zero-Hardcode Rule).
// T-20261106-thought-steps-call-the-model-under-the-planner.
var RoleFallbacks = map[domain.Purpose]domain.Purpose{
	domain.PurposeThought: domain.PurposePlanner,
	// The agentic retrieval planner follows the memory organ until an operator
	// binds a `retrieval_planner` role (owner decision 2026-11-14, third round;
	// T-20261102-agentic-planner-model-names-a-generator-id).
	domain.PurposeRetrievalPlanner: domain.PurposeMemory,
}

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

	// traceWrapper decorates every acquired generator with cross-cutting
	// observability (Langfuse), labelled by purpose. Injected from main so the
	// Provider stays decoupled from the premium layer. Nil = no tracing. Because
	// every LLM call flows through Acquire, wrapping here traces them all — no
	// per-call-site wrapping to forget (ADR-0042 + ADR-0019).
	traceWrapper func(gen domain.Generator, subsystem string) domain.Generator

	// runTracer records every call's full request and response as a JSONL span
	// (ADR-0145 D10, run_trace_layer.go). Nil = off (execution.trace.enabled).
	runTracer domain.RunTracer

	// sem bounds the number of concurrent in-flight LLM calls across EVERY call path
	// (agents + planner + verifier + agentic-retrieval + consolidator). Nil ⇒ no cap.
	// It composes with the LLMGateway CONWIP semaphore (which only gates agent calls);
	// this is the global backstop that stops direct system-organ calls from flooding a
	// rate-limited provider (HTTP 429). ADR-0042 chokepoint.
	sem chan struct{}

	// meter is the ADR-0131 D1 spend ledger, applied as the outermost decorator
	// in Acquire. Resolved per call (atomic), so generators handed out by
	// GeneratorFor BEFORE the composition root attached the meter are metered
	// too — app.go builds the organ generators long before the meter's config
	// consumer runs. Nil = no metering (maintenance binaries).
	meter atomic.Pointer[SpendMeter]

	// hooks is the ADR-0139 D11 pre_model/post_model chain (hook_layer.go),
	// fixed at construction (WithHooks). Nil = no hooks.
	hooks *domain.HookChain
	// finalAnswerTool / finalAnswerArg name the native tool lane's deliverable a
	// post_model hook sees and may rewrite (WithFinalAnswer; config defaults).
	finalAnswerTool string
	finalAnswerArg  string
	// guardedArgs are the control-verb arguments a post_model hook also sees and
	// may rewrite (WithGuardedArgs; execution.hooks.guarded_arguments defaults).
	guardedArgs []domain.GuardedArg

	// fallback is the ENG-453 fallback policy (owner decision 2026-11-14), fixed
	// at construction from llm_provider.pinned / equivalent_generators: which
	// generator may serve a call its first choice could not.
	fallback fallbackPolicy

	// contextMargin is llm_provider.context_margin_ratio with its default: the
	// share of a generator's context window the pre-send check keeps free
	// (contextGuardGenerator, ENG-1050).
	contextMargin float64

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
func NewProvider(cfg config.LLMProviderConfig, log *slog.Logger, opts ...ProviderOption) (*Provider, error) {
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
		// A RETIRED role (its organ was deleted) is dropped, not served: nothing
		// would ever acquire it, and listing it would show the operator a binding
		// that does nothing. Loading still succeeds — the file is not rewritten.
		if why, retired := RetiredRoles[r]; retired {
			log.Info("llm provider: ignoring retired role", "role", r, "generator", id, "why", why)
			continue
		}
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
	for _, group := range cfg.EquivalentGenerators {
		for _, id := range group {
			if _, ok := reg.Lookup(id); !ok {
				log.Warn("llm provider: equivalent_generators names an unknown generator; it can never serve a fallback",
					"generator", id)
			}
		}
	}
	log.Info("llm provider: fallback policy", "pinned", cfg.Pinned, "equivalence_groups", len(cfg.EquivalentGenerators),
		"rule", "a failed or unhealthy first choice falls back only to a generator marked equivalent; pinned: never")
	p := &Provider{
		fallback:      newFallbackPolicy(cfg.Pinned, cfg.EquivalentGenerators),
		contextMargin: cfg.ContextMargin(),
		breaker:       NewCircuitBreaker(cfg.Health.FailureThreshold, cooldown),
		ledger:        SeedPriceLedger(cfg.Generators),
		roles:         roles,
		sem:           sem,
		log:           log,
		// The defaults live with the config keys (execution.step_output).
		finalAnswerTool: config.StepOutputConfig{}.FinalAnswerToolName(),
		finalAnswerArg:  config.StepOutputConfig{}.FinalAnswerArgName(),
		guardedArgs:     config.HooksConfig{}.GuardedArgumentList(),
	}
	for _, o := range opts {
		o(p)
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

// SetSpendMeter attaches the ADR-0131 spend ledger. Every generator Acquire
// returns from then on — including those behind GeneratorFor handles created
// earlier — books its usage there and is refused at the session ceiling.
func (p *Provider) SetSpendMeter(m *SpendMeter) { p.meter.Store(m) }

// SpendMeter returns the attached spend ledger, or nil.
func (p *Provider) SpendMeter() *SpendMeter { return p.meter.Load() }

// Ledger exposes the price ledger (read/write) for cost wiring.
func (p *Provider) Ledger() *PriceLedger { return p.ledger }

// Registry exposes the generator registry for auction agent registration.
func (p *Provider) Registry() *GeneratorRegistry { return p.tab().registry }

// Default returns the global default generator id.
func (p *Provider) Default() string { return p.tab().defaultID }

// Acquire implements domain.LLMProvider: resolve a healthy model via the ladder,
// then return it wrapped in the health-recording decorator.
func (p *Provider) Acquire(ctx context.Context, req domain.LLMRequest) (domain.Generator, error) {
	gen, _, err := p.acquire(ctx, req, nil)
	return gen, err
}

// acquire is Acquire with the D1 fallback's exclusion set: generators that already
// failed THIS call with an endpoint-class error are skipped by the ladder even while
// their breaker is still closed. It also returns the resolved id.
func (p *Provider) acquire(ctx context.Context, req domain.LLMRequest, excluded map[string]bool) (domain.Generator, string, error) {
	id, err := p.resolveExcluding(req, excluded)
	if err != nil {
		if len(excluded) == 0 {
			p.log.Error("llm provider: no healthy model", "purpose", req.Purpose, "suggested", req.SuggestedModelID, "err", err)
		}
		return nil, "", err
	}
	entry, ok := p.tab().registry.Lookup(id)
	if !ok {
		return nil, "", fmt.Errorf("llm provider: resolved id %q not in registry", id)
	}
	purpose := string(req.Purpose)
	if purpose == "" {
		purpose = "model"
	}
	return p.decorate(entry, string(req.Purpose), purpose, true), id, nil
}

// decorate builds the Provider's decorator stack around one registered client. It
// is the ONE place the stack is assembled, shared by Acquire (the organs) and
// StreamOn/GenerateOn (the agent-plane gateway, ADR-0139 D1), so an agent call and
// an organ call cross the same breaker, tracing, concurrency cap and schema layer.
// metered=false omits the spend-meter layer for a caller that books the call
// itself against a lease (the gateway: ADR-0131 D3 admission and the per-request
// tally are lease-scoped, and metering here too would book every call twice).
func (p *Provider) decorate(entry GeneratorEntry, tracePurpose, meterPurpose string, metered bool) domain.Generator {
	id, client := entry.ID, entry.Generator
	// Health-recording (inner) so the breaker sees outcomes; tracing (outer) so
	// every call is observed by purpose. Tracing is a no-op when unset.
	gen := newHealthGenerator(id, client, p.breaker)
	// ADR-0145 D10: the run trace sits inside the schema-repair layer, so each
	// attempt is its own span with exactly what the model was sent.
	if p.runTracer != nil {
		gen = &runTraceGenerator{inner: gen, tracer: p.runTracer, id: id, purpose: tracePurpose}
	}
	if p.traceWrapper != nil {
		gen = p.traceWrapped(gen, tracePurpose)
	}
	// Outermost: the global concurrency cap gates the entire (traced, health-recorded)
	// call, so a burst of direct system-organ calls cannot flood the provider.
	if p.sem != nil {
		gen = &concurrencyGenerator{inner: gen, sem: p.sem}
	}
	// The pre-send context-window check (ENG-1050): outside the cap so a refused
	// call never holds a slot, inside the meter so it is never booked.
	if entry.ContextWindowTokens > 0 {
		gen = &contextGuardGenerator{inner: gen, id: id, window: entry.ContextWindowTokens,
			maxTokens: entry.MaxTokens, margin: p.contextMargin, log: p.log}
	}
	// Outermost of all: the ADR-0131 spend meter. Outside the concurrency cap so
	// a call refused at the session ceiling never occupies a slot; at Acquire so
	// every system organ is metered without any call site knowing.
	if m := p.meter.Load(); m != nil && metered {
		gen = &meteredGenerator{inner: gen, meter: m, purpose: meterPurpose, generatorID: id}
	}
	// The C1 response-schema layer (ADR-0139): validates a best-effort structured
	// reply and repairs it once. Outside the meter so each attempt is booked.
	return &schemaGenerator{inner: gen}
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

// NativeToolsEnabled forwards the inner generator's capability report (see
// healthGenerator.NativeToolsEnabled).
func (c *concurrencyGenerator) NativeToolsEnabled() bool {
	return domain.SupportsToolCalling(c.inner)
}

// GenerateWithTools forwards native tool-calling while holding a concurrency slot,
// mirroring Generate. See healthGenerator.GenerateWithTools on why forwarding is
// mandatory rather than optional.
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
//
// It takes no ctx on purpose (ENG-1467 item 6): the decision is an in-memory read
// of the published generator table, the role table and breaker health — no I/O,
// nothing scope- or session-dependent. The ctx parameter it used to carry was
// read by nothing, so capabilitiesFor had to invent one (context.Background()).
func (p *Provider) resolve(req domain.LLMRequest) (string, error) {
	return p.resolveExcluding(req, nil)
}

func (p *Provider) resolveExcluding(req domain.LLMRequest, excluded map[string]bool) (string, error) {
	// One table load for the whole decision: allIDs/default/capIndex are always
	// from the SAME published table, even mid-reload.
	t := p.tab()
	prefs := p.preferenceFor(req)
	// ENG-453: the ladder still decides ORDER, but only the call's first choice
	// (what the ladder would pick with every generator healthy) and the generators
	// marked equivalent to it may serve. Pinned admits the first choice alone.
	first := firstChoice(req.SuggestedModelID, req.CapabilityHints, prefs, t.allIDs, t.defaultID, t.capIndex)
	healthy := func(id string) bool {
		return !excluded[id] && p.fallback.admits(first, id) && p.breaker.Healthy(id)
	}
	id, err := resolveModel(
		req.SuggestedModelID,
		req.CapabilityHints,
		prefs,
		t.allIDs,
		t.defaultID,
		healthy,
		t.capIndex,
	)
	if err != nil && first != "" {
		return "", fmt.Errorf("%w: first choice %q is unavailable for this call and %s", err, first, p.fallback.describe(first))
	}
	return id, err
}

// preferenceFor returns the ordered preference ids for a request. System roles
// resolve through the role table; an agent step has no role preference here,
// so the ladder relies on suggestion -> default -> capability match. Agent-step
// model choice is made upstream by the auction's StepAllocation (ADR-0018) and
// served through the LLMGateway, not through this Provider: the ADR-0037 EFE
// ModelSelector hook that used to sit here was never wired and was removed
// with ADR-0037's supersession (ADR-0100). No model is hardcoded per task, so
// the Zero-Hardcode Rule holds.
func (p *Provider) preferenceFor(req domain.LLMRequest) []string {
	if req.Purpose == domain.PurposeAgentStep {
		return nil
	}
	// System role: deterministic role -> id (Zero-Hardcode-legal; roles are not agents).
	if id := p.RoleGenerator(req.Purpose); id != "" {
		return []string{id}
	}
	return nil
}

// RoleGenerator returns the generator id the role table binds purpose to, read
// per call so an operator's SetRole is live. A purpose with no role of its own
// resolves through its RoleFallbacks alias (thought -> planner,
// retrieval_planner -> memory), so splitting a purpose label never moves its
// traffic. "" when neither is bound (the caller's ladder serves the default).
func (p *Provider) RoleGenerator(purpose domain.Purpose) string {
	p.rolesMu.RLock()
	defer p.rolesMu.RUnlock()
	if id, ok := p.roles[string(purpose)]; ok {
		return id
	}
	if alias, has := RoleFallbacks[purpose]; has {
		return p.roles[string(alias)]
	}
	return ""
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

func (g *modelGenerator) request() domain.LLMRequest {
	return domain.LLMRequest{SuggestedModelID: g.id}
}

func (g *modelGenerator) Generate(ctx context.Context, prompt string) (string, error) {
	return g.provider.generatePrompt(ctx, g.request(), prompt)
}

type purposeGenerator struct {
	provider *Provider
	purpose  domain.Purpose
	hints    []string
}

func (g *purposeGenerator) request() domain.LLMRequest {
	return domain.LLMRequest{Purpose: g.purpose, CapabilityHints: g.hints}
}

func (g *purposeGenerator) Generate(ctx context.Context, prompt string) (string, error) {
	return g.provider.generatePrompt(ctx, g.request(), prompt)
}

// generatePrompt serves the legacy single-string port for both handle kinds,
// inside the D11 pre_model/post_model hooks (hook_layer.go).
func (p *Provider) generatePrompt(ctx context.Context, lr domain.LLMRequest, prompt string) (string, error) {
	label, model := lrLabel(lr), lr.SuggestedModelID
	prompt, err := p.prePrompt(ctx, label, model, prompt, false)
	if err != nil {
		return "", err
	}
	var out string
	err = p.withFallback(ctx, lr, func(gen domain.Generator) (err error) {
		out, err = gen.Generate(ctx, prompt)
		return err
	})
	if err != nil {
		return out, err
	}
	if err := p.postModel(ctx, label, model, domain.UserPrompt(prompt), &out, nil); err != nil {
		return "", err
	}
	return out, nil
}

// GenerateWithTools acquires a generator for this purpose and forwards native
// tool-calling. The acquired generator decides the capability, so this returns an
// error rather than silently degrading when the resolved generator cannot do it.
func (g *purposeGenerator) GenerateWithTools(
	ctx context.Context, messages []domain.ModelMessage, tools []domain.ToolDefinition,
) (domain.ModelTurn, error) {
	// D11: the same pre_model/post_model hooks as the C1 path.
	p, label := g.provider, string(g.purpose)
	hreq := domain.GenerateRequest{Messages: messages, Tools: tools}
	if err := p.preModel(ctx, label, "", &hreq, false); err != nil {
		return domain.ModelTurn{}, err
	}
	messages = hreq.Messages
	if len(hreq.System) > 0 {
		// Hook annotations. This port has no system slot, so the context leads the
		// first user turn (a copy — the caller's slice is untouched).
		messages = withLeadingContext(messages, hreq.System)
	}
	var out domain.ModelTurn
	err := p.withFallback(ctx, g.request(), func(gen domain.Generator) error {
		tg, ok := gen.(domain.ToolCallingGenerator)
		if !ok {
			return fmt.Errorf("llm provider: acquired %T does not implement native tool-calling", gen)
		}
		var err error
		out, err = tg.GenerateWithTools(ctx, messages, tools)
		return err
	})
	if err != nil {
		return out, err
	}
	if err := p.postModel(ctx, label, "", hreq, &out.Text, &out.ToolCalls); err != nil {
		return domain.ModelTurn{}, err
	}
	return out, nil
}

// GenerateStream acquires a healthy generator and delegates to its streaming
// surface, if any. Returns nil + a non-nil error if the inner generator
// does not implement streaming; callers can fall back to Generate. ADR-0042
// D5 live-failover applies identically to streaming calls.
func (g *purposeGenerator) GenerateStream(ctx context.Context, prompt string) (<-chan domain.StreamChunk, error) {
	if g.provider.postModelMatches(ctx, string(g.purpose)) {
		// A post_model hook matches: served buffered, so the hook sees the whole
		// reply before anything is delivered (hook_layer.go, bufferedPromptStream).
		prompt, err := g.provider.prePrompt(ctx, string(g.purpose), "", prompt, false)
		if err != nil {
			return nil, err
		}
		return g.provider.bufferedPromptStream(ctx, g.request(), prompt)
	}
	// D11: streamed calls no post_model hook matches fire pre_model only
	// (delivered chunks cannot be rewritten).
	prompt, err := g.provider.prePrompt(ctx, string(g.purpose), "", prompt, true)
	if err != nil {
		return nil, err
	}
	var out <-chan domain.StreamChunk
	err = g.provider.withFallback(ctx, g.request(), func(gen domain.Generator) error {
		sg, ok := gen.(streamingInner)
		if !ok {
			return fmt.Errorf("llm provider: generator %T does not implement streaming", gen)
		}
		var err error
		out, err = sg.GenerateStream(ctx, prompt)
		return err
	})
	return out, err
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

// Roles returns the CONFIGURED role → generator-id map (planner/verifier/router/
// memory/facts/thought). A purpose served through RoleFallbacks (thought with no
// role of its own) is not listed: it has no binding, it borrows one. Copied, so a caller cannot mutate the Provider's routing table.
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
