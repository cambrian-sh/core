package llm

import (
	"context"
	"fmt"
	"log/slog"
	"math"

	"github.com/cambrian-sh/core/domain"
)

// contextGuardGenerator is the pre-send context-window check
// (T-20261112-pre-send-context-window-check-per-model-with, owner decision
// 2026-11-18). Before a call leaves the kernel it estimates the prompt
// (EstimateTokens, content-aware) and refuses with domain.ErrContextOverflow when
//
//	estimate + output cap + margin × window > window
//
// so an over-window prompt is answered — classified exactly like the provider's
// own refusal (gateway OutOfRange, deterministic, no fallback, no breaker event) —
// WITHOUT spending a call. The output cap is the request's max_tokens, else the
// generator's configured max_tokens, else 0 (the provider's default is unknown).
//
// It sits in Provider.decorate between the spend meter and the concurrency cap:
// inside the meter so a refused call is never booked, outside the cap so it never
// holds a slot, and inside the schema layer so a repair attempt is checked too.
// The window is the generator's: after a fallback the equivalent model's own
// guard decides with its own window. Installed only for a generator whose
// context_window_tokens is set (0 = unknown = no check).
type contextGuardGenerator struct {
	inner     domain.Generator
	id        string
	window    int
	maxTokens int // the generator's configured output cap (0 = provider default)
	margin    float64
	log       *slog.Logger
}

// check returns domain.ErrContextOverflow (wrapped with the numbers) when the
// prompt text plus the output cap does not fit the window less the margin.
func (g *contextGuardGenerator) check(text string, reqMaxTokens int) error {
	out := reqMaxTokens
	if out <= 0 {
		out = g.maxTokens
	}
	reserve := int(math.Ceil(g.margin * float64(g.window)))
	budget := g.window - out - reserve
	est := EstimateTokens(text)
	if est <= budget {
		return nil
	}
	if g.log != nil {
		g.log.Warn("llm provider: prompt refused before sending; it does not fit the model's context window",
			"generator", g.id, "estimated_prompt_tokens", est, "context_window_tokens", g.window,
			"output_cap_tokens", out, "margin_tokens", reserve)
	}
	return fmt.Errorf("llm provider: generator %q: %w (estimated %d prompt tokens + %d output + %d margin > %d window; refused before sending)",
		g.id, domain.ErrContextOverflow, est, out, reserve, g.window)
}

func (g *contextGuardGenerator) Generate(ctx context.Context, prompt string) (string, error) {
	if err := g.check(prompt, 0); err != nil {
		return "", err
	}
	return g.inner.Generate(ctx, prompt)
}

// NativeToolsEnabled forwards the inner generator's capability report (see
// healthGenerator.NativeToolsEnabled).
func (g *contextGuardGenerator) NativeToolsEnabled() bool { return domain.SupportsToolCalling(g.inner) }

func (g *contextGuardGenerator) GenerateWithTools(
	ctx context.Context, messages []domain.ModelMessage, tools []domain.ToolDefinition,
) (domain.ModelTurn, error) {
	tg, ok := g.inner.(domain.ToolCallingGenerator)
	if !ok {
		return domain.ModelTurn{}, fmt.Errorf("llm provider: %T does not implement native tool-calling", g.inner)
	}
	if err := g.check(RequestText(domain.GenerateRequest{Messages: messages, Tools: tools}), 0); err != nil {
		return domain.ModelTurn{}, err
	}
	return tg.GenerateWithTools(ctx, messages, tools)
}

func (g *contextGuardGenerator) GenerateStream(ctx context.Context, prompt string) (<-chan domain.StreamChunk, error) {
	sg, ok := g.inner.(streamingInner)
	if !ok {
		return nil, fmt.Errorf("llm provider: %T does not implement streaming", g.inner)
	}
	if err := g.check(prompt, 0); err != nil {
		return nil, err
	}
	return sg.GenerateStream(ctx, prompt)
}

func (g *contextGuardGenerator) GenerateRequest(ctx context.Context, req domain.GenerateRequest) (domain.GenerateResponse, error) {
	rg, ok := domain.AsRequestGenerator(g.inner)
	if !ok {
		return domain.GenerateResponse{}, fmt.Errorf("llm provider: %T does not implement GenerateRequest", g.inner)
	}
	if err := g.check(RequestText(req), req.Options.MaxTokens); err != nil {
		return domain.GenerateResponse{}, err
	}
	return rg.GenerateRequest(ctx, req)
}

func (g *contextGuardGenerator) StreamRequest(ctx context.Context, req domain.GenerateRequest) (<-chan domain.StreamChunk, error) {
	rg, ok := domain.AsRequestGenerator(g.inner)
	if !ok {
		return nil, fmt.Errorf("llm provider: %T does not implement StreamRequest", g.inner)
	}
	if err := g.check(RequestText(req), req.Options.MaxTokens); err != nil {
		return nil, err
	}
	return rg.StreamRequest(ctx, req)
}

func (g *contextGuardGenerator) Capabilities() domain.Capabilities { return capabilitiesOf(g.inner) }

func (g *contextGuardGenerator) RequestsEnabled() bool {
	_, ok := domain.AsRequestGenerator(g.inner)
	return ok
}

var (
	_ domain.Generator            = (*contextGuardGenerator)(nil)
	_ domain.ToolCallingGenerator = (*contextGuardGenerator)(nil)
	_ domain.RequestGenerator     = (*contextGuardGenerator)(nil)
)
