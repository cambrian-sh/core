package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/cambrian-sh/core/domain"
)

// The C1 surface of the Provider's decorator stack (ADR-0139 D1).
//
// Every wrapper the Provider puts around a client — health (breaker), tracing,
// concurrency, spend meter, response schema — must forward RequestGenerator AND its
// report, or a capable generator is silently downgraded to the legacy string path.
// That is the same silent-wiring shape ADR-0097 D1 guards against for tool calling,
// and it has the same answer: forwarding methods here, one test per decorator.

// ── Concurrency cap ──────────────────────────────────────────────────────────

func (c *concurrencyGenerator) GenerateRequest(ctx context.Context, req domain.GenerateRequest) (domain.GenerateResponse, error) {
	rg, ok := domain.AsRequestGenerator(c.inner)
	if !ok {
		return domain.GenerateResponse{}, fmt.Errorf("llm provider: %T does not implement GenerateRequest", c.inner)
	}
	if err := c.acquire(ctx); err != nil {
		return domain.GenerateResponse{}, err
	}
	defer func() { <-c.sem }()
	return rg.GenerateRequest(ctx, req)
}

func (c *concurrencyGenerator) StreamRequest(ctx context.Context, req domain.GenerateRequest) (<-chan domain.StreamChunk, error) {
	rg, ok := domain.AsRequestGenerator(c.inner)
	if !ok {
		return nil, fmt.Errorf("llm provider: %T does not implement StreamRequest", c.inner)
	}
	if err := c.acquire(ctx); err != nil {
		return nil, err
	}
	in, err := rg.StreamRequest(ctx, req)
	if err != nil {
		<-c.sem
		return nil, err
	}
	out := make(chan domain.StreamChunk, 64)
	go func() {
		defer func() { <-c.sem }() // held until the whole stream is drained
		defer close(out)
		for chunk := range in {
			out <- chunk
		}
	}()
	return out, nil
}

func (c *concurrencyGenerator) Capabilities() domain.Capabilities { return capabilitiesOf(c.inner) }

func (c *concurrencyGenerator) RequestsEnabled() bool {
	_, ok := domain.AsRequestGenerator(c.inner)
	return ok
}

// ── Spend meter ──────────────────────────────────────────────────────────────

func (g *meteredGenerator) GenerateRequest(ctx context.Context, req domain.GenerateRequest) (domain.GenerateResponse, error) {
	rg, ok := domain.AsRequestGenerator(g.inner)
	if !ok {
		return domain.GenerateResponse{}, fmt.Errorf("llm provider: metered %T does not implement GenerateRequest", g.inner)
	}
	prompt := RequestText(req)
	if err := g.meter.Admit(ctx, g.purpose, EstimateTokens(prompt)); err != nil {
		return domain.GenerateResponse{}, err
	}
	resp, err := rg.GenerateRequest(ctx, req)
	if err != nil {
		return resp, err
	}
	g.meter.Record(ctx, g.purpose, g.generatorID, ResponseUsage(prompt, resp))
	return resp, nil
}

func (g *meteredGenerator) StreamRequest(ctx context.Context, req domain.GenerateRequest) (<-chan domain.StreamChunk, error) {
	rg, ok := domain.AsRequestGenerator(g.inner)
	if !ok {
		return nil, fmt.Errorf("llm provider: metered %T does not implement StreamRequest", g.inner)
	}
	prompt := RequestText(req)
	if err := g.meter.Admit(ctx, g.purpose, EstimateTokens(prompt)); err != nil {
		return nil, err
	}
	in, err := rg.StreamRequest(ctx, req)
	if err != nil {
		return nil, err
	}
	out := make(chan domain.StreamChunk, 64)
	go func() {
		defer close(out)
		var text strings.Builder
		var usage StreamUsage
		var final *domain.GenerateResponse
		for chunk := range in {
			text.WriteString(chunk.Text)
			usage.Observe(chunk)
			if chunk.Response != nil {
				final = chunk.Response
			}
			out <- chunk
		}
		if final != nil {
			g.meter.Record(ctx, g.purpose, g.generatorID, ResponseUsage(prompt, *final))
			return
		}
		g.meter.Record(ctx, g.purpose, g.generatorID, ReconcileTokens(prompt, usage.Chunk(), text.String()))
	}()
	return out, nil
}

func (g *meteredGenerator) Capabilities() domain.Capabilities { return capabilitiesOf(g.inner) }

func (g *meteredGenerator) RequestsEnabled() bool {
	_, ok := domain.AsRequestGenerator(g.inner)
	return ok
}

// ResponseUsage is the provider's usage for a C1 response, or the flagged len/4
// estimate when the provider reported none (ADR-0131 D2).
func ResponseUsage(prompt string, resp domain.GenerateResponse) TokenUsage {
	if u := tokenUsageOf(resp.Usage); !u.IsZero() {
		return u
	}
	return EstimateUsage(prompt, TurnText(resp.Turn()))
}

// ── Tracing ──────────────────────────────────────────────────────────────────

var traceBypassWarn sync.Once

// traceWrapped applies the injected trace wrapper. The premium Langfuse wrapper
// predates C1 and exposes only Generate and GenerateStream, which would hide the
// C1 request path of every acquired generator. Rather than downgrade silently,
// C1 calls then go to the untraced (health-recorded) generator — and the gap is
// logged once — while every legacy surface the wrapper does expose stays traced
// and nothing it hides is newly exposed.
func (p *Provider) traceWrapped(gen domain.Generator, purpose string) domain.Generator {
	traced := p.traceWrapper(gen, purpose)
	if _, ok := domain.AsRequestGenerator(traced); ok {
		return traced
	}
	if _, ok := domain.AsRequestGenerator(gen); !ok {
		return traced
	}
	traceBypassWarn.Do(func() {
		p.log.Warn("llm provider: the trace wrapper does not forward the C1 RequestGenerator; " +
			"GenerateRequest/StreamRequest calls are NOT traced until it does (ADR-0139 D1)")
	})
	return &traceBypass{traced: traced, untraced: gen}
}

// traceBypass serves the legacy ports through the trace wrapper and C1 around it.
type traceBypass struct {
	traced   domain.Generator
	untraced domain.Generator
}

func (t *traceBypass) Generate(ctx context.Context, prompt string) (string, error) {
	return t.traced.Generate(ctx, prompt)
}

// NativeToolsEnabled reports exactly what the traced wrapper exposes: a wrapper
// that hides tool calling keeps hiding it (no behaviour change for its users).
func (t *traceBypass) NativeToolsEnabled() bool { return domain.SupportsToolCalling(t.traced) }

func (t *traceBypass) GenerateWithTools(ctx context.Context, messages []domain.ModelMessage, tools []domain.ToolDefinition) (domain.ModelTurn, error) {
	tg, ok := t.traced.(domain.ToolCallingGenerator)
	if !ok {
		return domain.ModelTurn{}, fmt.Errorf("llm provider: traced %T does not implement native tool-calling", t.traced)
	}
	return tg.GenerateWithTools(ctx, messages, tools)
}

func (t *traceBypass) GenerateStream(ctx context.Context, prompt string) (<-chan domain.StreamChunk, error) {
	sg, ok := t.traced.(streamingInner)
	if !ok {
		return nil, fmt.Errorf("llm provider: traced %T does not implement streaming", t.traced)
	}
	return sg.GenerateStream(ctx, prompt)
}

func (t *traceBypass) GenerateRequest(ctx context.Context, req domain.GenerateRequest) (domain.GenerateResponse, error) {
	rg, _ := domain.AsRequestGenerator(t.untraced)
	return rg.GenerateRequest(ctx, req)
}

func (t *traceBypass) StreamRequest(ctx context.Context, req domain.GenerateRequest) (<-chan domain.StreamChunk, error) {
	rg, _ := domain.AsRequestGenerator(t.untraced)
	return rg.StreamRequest(ctx, req)
}

func (t *traceBypass) Capabilities() domain.Capabilities { return capabilitiesOf(t.untraced) }
func (t *traceBypass) RequestsEnabled() bool             { return true }

// ── Purpose- and model-bound handles ─────────────────────────────────────────

func (g *purposeGenerator) GenerateRequest(ctx context.Context, req domain.GenerateRequest) (domain.GenerateResponse, error) {
	return g.provider.generateRequest(ctx, g.request(), req)
}

func (g *purposeGenerator) StreamRequest(ctx context.Context, req domain.GenerateRequest) (<-chan domain.StreamChunk, error) {
	return g.provider.streamRequest(ctx, g.request(), req)
}

// Capabilities reports what the generator this handle would resolve to NOW
// declares. Resolution is per call, so a later call may land on another generator;
// callers that need a guarantee mark the field strict and get ErrUnsupported.
func (g *purposeGenerator) Capabilities() domain.Capabilities {
	return g.provider.capabilitiesFor(g.request())
}

func (g *purposeGenerator) RequestsEnabled() bool { return true }

func (g *modelGenerator) GenerateRequest(ctx context.Context, req domain.GenerateRequest) (domain.GenerateResponse, error) {
	return g.provider.generateRequest(ctx, g.request(), req)
}

func (g *modelGenerator) StreamRequest(ctx context.Context, req domain.GenerateRequest) (<-chan domain.StreamChunk, error) {
	return g.provider.streamRequest(ctx, g.request(), req)
}

func (g *modelGenerator) Capabilities() domain.Capabilities {
	return g.provider.capabilitiesFor(g.request())
}
func (g *modelGenerator) RequestsEnabled() bool { return true }

// generateRequest serves a C1 call on a purpose/model handle, inside the D11
// pre_model/post_model hooks (hook_layer.go) — outside the fallback walk, so a
// hook sees one logical call.
func (p *Provider) generateRequest(ctx context.Context, lr domain.LLMRequest, req domain.GenerateRequest) (domain.GenerateResponse, error) {
	label, model := lrLabel(lr), lr.SuggestedModelID
	if err := p.preModel(ctx, label, model, &req, false); err != nil {
		return domain.GenerateResponse{}, err
	}
	var out domain.GenerateResponse
	err := p.withFallback(ctx, lr, func(gen domain.Generator) error {
		rg, ok := domain.AsRequestGenerator(gen)
		if !ok {
			return fmt.Errorf("llm provider: acquired %T does not implement GenerateRequest", gen)
		}
		var err error
		out, err = rg.GenerateRequest(ctx, req)
		return err
	})
	if err != nil {
		return out, err
	}
	if err := p.postModel(ctx, label, model, req, &out.Text, &out.ToolCalls); err != nil {
		return domain.GenerateResponse{}, err
	}
	return out, nil
}

// streamRequest fires pre_model only: chunks already delivered cannot be
// rewritten or withheld (D11) — unless a post_model hook's scope matches the call,
// which is then served buffered (bufferedStream, hook_layer.go).
func (p *Provider) streamRequest(ctx context.Context, lr domain.LLMRequest, req domain.GenerateRequest) (<-chan domain.StreamChunk, error) {
	label, model := lrLabel(lr), lr.SuggestedModelID
	if p.postModelMatches(ctx, label) {
		if err := p.preModel(ctx, label, model, &req, false); err != nil {
			return nil, err
		}
		out, _, err := p.bufferedStream(ctx, label, model, req, func(call func(domain.Generator) error) (string, error) {
			return "", p.withFallback(ctx, lr, call)
		})
		return out, err
	}
	if err := p.preModel(ctx, label, model, &req, true); err != nil {
		return nil, err
	}
	var out <-chan domain.StreamChunk
	err := p.withFallback(ctx, lr, func(gen domain.Generator) error {
		rg, ok := domain.AsRequestGenerator(gen)
		if !ok {
			return fmt.Errorf("llm provider: acquired %T does not implement StreamRequest", gen)
		}
		var err error
		out, err = rg.StreamRequest(ctx, req)
		return err
	})
	return out, err
}

func (p *Provider) capabilitiesFor(lr domain.LLMRequest) domain.Capabilities {
	id, err := p.resolve(lr)
	if err != nil {
		return domain.Capabilities{}
	}
	entry, ok := p.tab().registry.Lookup(id)
	if !ok {
		return domain.Capabilities{}
	}
	return capabilitiesOf(entry.Generator)
}

var (
	_ domain.RequestGenerator = (*purposeGenerator)(nil)
	_ domain.RequestGenerator = (*modelGenerator)(nil)
	_ domain.RequestGenerator = (*meteredGenerator)(nil)
	_ domain.RequestGenerator = (*concurrencyGenerator)(nil)
	_ domain.RequestGenerator = (*healthGenerator)(nil)
	_ domain.RequestGenerator = (*traceBypass)(nil)
	_ domain.RequestGenerator = (*schemaGenerator)(nil)
)

// ── Fallback (ADR-0139 D1) ───────────────────────────────────────────────────

// withFallback runs call on a freshly acquired generator and, when it fails with
// an endpoint-class error, on the next generator the ladder offers:
//
//   - 429 and 5xx (after sharedLLMTransport's own backoff gave up) and transport
//     failures switch provider at once;
//   - a refused credential (401/403 or its streamed form, domain.ErrProviderAuth)
//     is neither retried nor switched (owner decision 2026-11-18): it ends the call
//     until a person fixes the credential — every call re-reads it (APIKeyFor);
//   - a refusal (a successful response with StopRefusal), a schema failure
//     (domain.ErrSchemaValidation), an unsupported field, a 4xx request error and
//     the caller's own cancellation never switch: another provider would answer the
//     same request the same way, or the caller has stopped listening.
//
// The ladder skips the generators that already failed this call, so the walk is
// bounded by the number of configured generators. With one generator it is the old
// behaviour exactly: the first error is returned.
func (p *Provider) withFallback(ctx context.Context, lr domain.LLMRequest, call func(domain.Generator) error) error {
	_, err := p.fallbackLoop(ctx, string(lr.Purpose), func(excluded map[string]bool) (domain.Generator, string, error) {
		return p.acquire(ctx, lr, excluded)
	}, call)
	return err
}

// fallbackLoop is withFallback over any acquisition rule: the purpose ladder for the
// organs, a step's fixed candidate chain for the gateway. It returns the id of the
// generator whose call ended the walk (successfully or not).
func (p *Provider) fallbackLoop(ctx context.Context, label string, acquire func(excluded map[string]bool) (domain.Generator, string, error), call func(domain.Generator) error) (string, error) {
	var excluded map[string]bool
	var lastErr error
	var lastFailed string
	for {
		gen, id, err := acquire(excluded)
		if err != nil {
			if lastErr != nil {
				// Nothing left to fall back to: the provider's own error is the
				// useful one, not "no healthy generator". Loud (ENG-453): with the
				// fallback restricted to equivalent generators, this is where a dead
				// backbone stops a run instead of being papered over by another model.
				p.log.Error("llm provider: generator failed and nothing may serve the call in its place",
					"purpose", label, "failed", lastFailed, "why", err, "err", lastErr)
				return "", lastErr
			}
			return "", err
		}
		err = call(gen)
		if errors.Is(err, domain.ErrProviderAuth) {
			// Loud: nothing in the kernel can fix a refused credential, and no other
			// model is tried in its place.
			p.log.Error("llm provider: the provider refused the credential; not retrying, not falling back",
				"purpose", label, "generator", id, "err", err)
		}
		if classifyForFallback(ctx, err) != fallbackNow {
			return id, err
		}
		if excluded == nil {
			excluded = make(map[string]bool)
		}
		excluded[id] = true
		lastErr, lastFailed = err, id
		p.log.Warn("llm provider: generator unavailable for this call; falling back",
			"purpose", label, "generator", id, "err", err)
	}
}

// ── Agent-plane step chains (ADR-0139 D1, wave 1c) ───────────────────────────

// GenerateOn serves one agent-plane call (the LLM gateway) on a step's model chain.
//
// candidates are generator ids in preference order — the auction's winner, its
// fallbacks, then the configured default. The first one the circuit breaker admits
// serves the call; on an endpoint-class failure (transport, 429, 5xx) the next one
// does. A refusal, a schema failure, an unsupported field, a refused credential
// (401/403) or a 4xx request error never moves down the chain: the same D1 policy,
// and the same withFallback walk, the organs use.
//
// The call crosses the Provider's whole decorator stack EXCEPT the spend meter:
// the gateway admits and books lease calls itself (ADR-0131 D3), so metering here
// too would book every agent call twice. It returns the id of the generator that
// served (or last failed) the call.
func (p *Provider) GenerateOn(ctx context.Context, candidates []string, req domain.GenerateRequest) (domain.GenerateResponse, string, error) {
	// D11 hooks around the whole chain walk (hook_layer.go).
	label, model := string(domain.PurposeAgentStep), firstCandidate(candidates)
	if err := p.preModel(ctx, label, model, &req, false); err != nil {
		return domain.GenerateResponse{}, "", err
	}
	var out domain.GenerateResponse
	id, err := p.fallbackLoop(ctx, string(domain.PurposeAgentStep), p.chainAcquirer(candidates), func(gen domain.Generator) error {
		rg, ok := domain.AsRequestGenerator(gen)
		if !ok {
			return fmt.Errorf("llm provider: acquired %T does not implement GenerateRequest", gen)
		}
		var err error
		out, err = rg.GenerateRequest(ctx, req)
		return err
	})
	if err != nil {
		return out, id, err
	}
	if err := p.postModel(ctx, label, model, req, &out.Text, &out.ToolCalls); err != nil {
		return domain.GenerateResponse{}, id, err
	}
	return out, id, nil
}

// GenerateOnObserved is GenerateOn served over the generator's STREAM, reporting
// each text delta and tool-call fragment to observe while the call runs (ADR-0139
// amendment 2026-11-06: the native tool lane streams internally so the step's live
// output reaches the operator feed; the agent still gets one unary response).
//
// Everything GenerateOn guarantees holds: the D11 pre_model hooks run before the
// call and the post_model hooks run on the COMPLETE reply afterwards — its text and
// its tool calls, the final-answer deliverable included (this is not a
// "streamed call" to a hook — post_model fires, and its rewrite or refusal governs
// what the caller receives); the fallback walk is the same, and a stream that
// breaks mid-way is an endpoint failure like any other, so the next candidate
// serves — observe then gets a Restarted delta, because what it saw so far belongs
// to the abandoned attempt. Deltas are reported BEFORE post_model runs, so a caller
// that must not leak pre-hook text must not observe (the gateway's step-output
// stream suppresses itself whenever a post_model hook is installed).
//
// A generator whose StreamRequest answers domain.ErrUnsupported is served unary on
// the same attempt, with no deltas. observe == nil is GenerateOn.
// observe runs on the calling goroutine and must be prompt and non-blocking.
func (p *Provider) GenerateOnObserved(ctx context.Context, candidates []string, req domain.GenerateRequest, observe func(domain.ModelStreamDelta)) (domain.GenerateResponse, string, error) {
	if observe == nil {
		return p.GenerateOn(ctx, candidates, req)
	}
	label, model := string(domain.PurposeAgentStep), firstCandidate(candidates)
	if err := p.preModel(ctx, label, model, &req, false); err != nil {
		return domain.GenerateResponse{}, "", err
	}
	var out domain.GenerateResponse
	emitted := false
	id, err := p.fallbackLoop(ctx, string(domain.PurposeAgentStep), p.chainAcquirer(candidates), func(gen domain.Generator) error {
		rg, ok := domain.AsRequestGenerator(gen)
		if !ok {
			return fmt.Errorf("llm provider: acquired %T does not implement StreamRequest", gen)
		}
		if emitted {
			observe(domain.ModelStreamDelta{Restarted: true})
			emitted = false
		}
		resp, sawDelta, err := consumeObservedStream(ctx, rg, req, observe)
		emitted = emitted || sawDelta
		if err != nil {
			return err
		}
		out = resp
		return nil
	})
	if err != nil {
		return out, id, err
	}
	if err := p.postModel(ctx, label, model, req, &out.Text, &out.ToolCalls); err != nil {
		return domain.GenerateResponse{}, id, err
	}
	return out, id, nil
}

// consumeObservedStream runs one streamed attempt to its end, reporting deltas,
// and returns the final chunk's complete response. The adapters' stream_error
// convention (a final chunk with text and no Response) is an error here, never a
// delta. It always drains the channel, so every layer down to the adapter closes.
func consumeObservedStream(ctx context.Context, rg domain.RequestGenerator, req domain.GenerateRequest, observe func(domain.ModelStreamDelta)) (domain.GenerateResponse, bool, error) {
	ch, err := rg.StreamRequest(ctx, req)
	if err != nil {
		// Not streamable (or a field it refuses): serve the attempt unary. A refused
		// field is refused again by GenerateRequest before any model is called, so
		// the caller sees the same C1 refusal it would have seen from GenerateOn.
		if errors.Is(err, domain.ErrUnsupported) {
			resp, gerr := rg.GenerateRequest(ctx, req)
			return resp, false, gerr
		}
		return domain.GenerateResponse{}, false, err
	}
	var final *domain.GenerateResponse
	var streamErr string
	var typedErr error
	saw := false
	for c := range ch {
		if c.Response != nil {
			final = c.Response
		}
		if c.IsFinal && c.Response == nil {
			streamErr = c.Text
			typedErr = c.Err
			continue
		}
		if c.Text == "" && c.ToolCall == nil {
			continue
		}
		saw = true
		observe(domain.ModelStreamDelta{Text: c.Text, ToolCall: c.ToolCall})
	}
	if err := ctx.Err(); err != nil {
		return domain.GenerateResponse{}, saw, err
	}
	if final == nil {
		if typedErr != nil {
			// Keep the identity (domain.ErrContextOverflow must not become a
			// generic stream failure the fallback policy reads as an outage).
			return domain.GenerateResponse{}, saw, typedErr
		}
		if streamErr == "" {
			streamErr = "stream ended without a final response"
		}
		return domain.GenerateResponse{}, saw, errors.New(streamErr)
	}
	return *final, saw, nil
}

// firstCandidate is the step chain's preferred generator id, for hook events.
func firstCandidate(candidates []string) string {
	for _, c := range candidates {
		if c != "" {
			return c
		}
	}
	return ""
}

// StreamOn is GenerateOn for a streamed call. Fallback covers OPENING the stream;
// once chunks flow, a failure ends that stream (a partial answer cannot be resumed
// on another model).
//
// When a D11 post_model hook's scope matches the call (agent_step, the context's
// agent) the call is served BUFFERED instead — the same shape as
// GenerateOnObserved: the provider stream is consumed here, post_model runs on the
// complete reply, and the returned channel holds only the (possibly rewritten)
// reply, already closed. This is what puts the SDK's text protocol (served by
// GenerateViaModelStream) under post_model, its final answer and control-verb
// envelopes included (T-20261106-text-protocol-agent-replies-never-reach-post).
// A refusal or a failed assembly returns an error and no channel.
func (p *Provider) StreamOn(ctx context.Context, candidates []string, req domain.GenerateRequest) (<-chan domain.StreamChunk, string, error) {
	label, model := string(domain.PurposeAgentStep), firstCandidate(candidates)
	walk := func(call func(domain.Generator) error) (string, error) {
		return p.fallbackLoop(ctx, label, p.chainAcquirer(candidates), call)
	}
	if p.postModelMatches(ctx, label) {
		if err := p.preModel(ctx, label, model, &req, false); err != nil {
			return nil, "", err
		}
		return p.bufferedStream(ctx, label, model, req, walk)
	}
	// D11: a streamed call no post_model hook matches fires pre_model only.
	if err := p.preModel(ctx, label, model, &req, true); err != nil {
		return nil, "", err
	}
	var out <-chan domain.StreamChunk
	id, err := p.fallbackLoop(ctx, string(domain.PurposeAgentStep), p.chainAcquirer(candidates), func(gen domain.Generator) error {
		rg, ok := domain.AsRequestGenerator(gen)
		if !ok {
			return fmt.Errorf("llm provider: acquired %T does not implement StreamRequest", gen)
		}
		var err error
		out, err = rg.StreamRequest(ctx, req)
		return err
	})
	return out, id, err
}

// chainAcquirer acquires the first candidate that is registered, not already
// failed this call, and admitted by the breaker. Unknown ids are skipped (a
// generator removed by a live reload, or an allocation naming no generator).
func (p *Provider) chainAcquirer(candidates []string) func(excluded map[string]bool) (domain.Generator, string, error) {
	return func(excluded map[string]bool) (domain.Generator, string, error) {
		reg := p.tab().registry
		// ENG-453: the chain's first REGISTERED candidate is the step's own model;
		// a later candidate serves only when marked equivalent to it, and never
		// when llm_provider.pinned.
		first := ""
		for _, id := range candidates {
			if _, ok := reg.Lookup(id); id != "" && ok {
				first = id
				break
			}
		}
		var skipped []string
		for _, id := range candidates {
			if id == "" || excluded[id] {
				continue
			}
			entry, ok := reg.Lookup(id)
			if !ok {
				skipped = append(skipped, id+": not a configured generator")
				continue
			}
			if !p.fallback.admits(first, id) {
				skipped = append(skipped, id+": not a fallback for "+first+" ("+p.fallback.describe(first)+")")
				continue
			}
			if !p.breaker.Healthy(id) {
				skipped = append(skipped, id+": circuit open")
				continue
			}
			return p.decorate(id, entry.Generator, string(domain.PurposeAgentStep), "", false), id, nil
		}
		return nil, "", fmt.Errorf("%w: candidates %v (%s)", ErrNoHealthyModel, candidates, strings.Join(skipped, "; "))
	}
}
