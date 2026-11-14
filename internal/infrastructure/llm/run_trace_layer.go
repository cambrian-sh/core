package llm

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cambrian-sh/core/domain"
)

// The run-trace layer (ADR-0145 D10, ADR-0139 C6): every model call that crosses
// the Provider — the organs through Acquire (planner, thought steps, router, …) and
// the agent-plane gateway through StreamOn/GenerateOn (chat and worker agents) —
// is recorded as a "chat {model}" span with its full request (system
// instructions, messages, tools offered) and response, preceded by a start event
// carrying the request when the call begins (so a follower sees it while it runs). Installed by decorate
// right outside the health layer when execution.trace.enabled wired a tracer
// (SetRunTracer); absent otherwise, so the default path is unchanged.
//
// Like every decorator here it forwards the legacy ports, native tool calling,
// streaming and the C1 RequestGenerator with its capability report, or a capable
// generator would be silently downgraded (request_layers.go).

type runTraceGenerator struct {
	inner   domain.Generator
	tracer  domain.RunTracer
	id      string
	purpose string
}

// SetRunTracer installs the run tracer. Bootstrap-time; nil leaves tracing off.
func (p *Provider) SetRunTracer(t domain.RunTracer) {
	p.runTracer = t
}

// requestAttrs are what the model was sent: written on the start event when the
// call begins and again on the finished span.
func (g *runTraceGenerator) requestAttrs(req domain.GenerateRequest) map[string]any {
	attrs := map[string]any{
		domain.GenAIOperationName: domain.GenAIOpChat,
		domain.GenAIRequestModel:  g.id,
		"cambrian.generator.id":   g.id,
		"cambrian.purpose":        g.purpose,
		domain.GenAIInputMessages: domain.GenAIMessages(req.Messages),
	}
	if len(req.System) > 0 {
		sys := make([]map[string]any, 0, len(req.System))
		for _, b := range req.System {
			sys = append(sys, map[string]any{"type": "text", "content": b.Text})
		}
		attrs[domain.GenAISystemInstructions] = sys
	}
	if len(req.Tools) > 0 {
		defs := make([]map[string]any, 0, len(req.Tools))
		names := make([]string, 0, len(req.Tools))
		for _, t := range req.Tools {
			defs = append(defs, map[string]any{"type": "function", "name": t.Name,
				"description": t.Description, "parameters": string(t.Parameters)})
			names = append(names, t.Name)
		}
		attrs[domain.GenAIToolDefinitions] = defs
		attrs["cambrian.tools.offered"] = names
	}
	if req.ToolChoice.Mode != "" || req.ToolChoice.Name != "" {
		attrs["cambrian.request.tool_choice"] = strings.TrimSpace(string(req.ToolChoice.Mode) + " " + req.ToolChoice.Name)
	}
	if req.ResponseSchema != nil {
		attrs["cambrian.request.response_schema"] = string(req.ResponseSchema.Schema)
		if req.ResponseSchema.Name != "" {
			attrs["cambrian.request.response_schema_name"] = req.ResponseSchema.Name
		}
	}
	return attrs
}

// begin writes the call's start event (the request, "cambrian.phase": "start")
// so a follower of the trace sees the call while it runs, and returns the span id
// and start time the finished span carries.
func (g *runTraceGenerator) begin(ctx context.Context, req domain.GenerateRequest) (string, time.Time) {
	return domain.StartTraceSpan(ctx, g.tracer, domain.TraceSpan{Name: "chat " + g.id, Kind: "CLIENT",
		Attributes: g.requestAttrs(req)})
}

func (g *runTraceGenerator) span(ctx context.Context, id string, start time.Time, req domain.GenerateRequest, resp domain.GenerateResponse, err error) {
	attrs := g.requestAttrs(req)
	s := domain.TraceSpan{Name: "chat " + g.id, Kind: "CLIENT", Start: start, End: time.Now(), Attributes: attrs, SpanID: id}
	if err != nil {
		s.Error = err.Error()
	} else {
		out := map[string]any{"role": "assistant"}
		var parts []map[string]any
		if resp.Text != "" {
			parts = append(parts, map[string]any{"type": "text", "content": resp.Text})
		}
		for _, c := range resp.ToolCalls {
			parts = append(parts, map[string]any{"type": "tool_call", "id": c.ID, "name": c.Name, "arguments": string(c.Arguments)})
		}
		out["parts"] = parts
		out["finish_reason"] = string(resp.StopReason)
		attrs[domain.GenAIOutputMessages] = []map[string]any{out}
		attrs[domain.GenAIResponseFinish] = []string{string(resp.StopReason)}
		if resp.Model != "" {
			attrs[domain.GenAIResponseModel] = resp.Model
		}
		attrs[domain.GenAIUsageInputTokens] = resp.Usage.Input
		attrs[domain.GenAIUsageOutputTokens] = resp.Usage.Output
		attrs[domain.GenAIUsageCacheReadInput] = resp.Usage.CacheRead
		if len(resp.Ignored) > 0 {
			attrs["cambrian.response.ignored"] = resp.Ignored
		}
	}
	g.tracer.Span(ctx, s)
}

func (g *runTraceGenerator) Generate(ctx context.Context, prompt string) (string, error) {
	req := domain.UserPrompt(prompt)
	id, start := g.begin(ctx, req)
	out, err := g.inner.Generate(ctx, prompt)
	g.span(ctx, id, start, req, domain.GenerateResponse{Text: out}, err)
	return out, err
}

// NativeToolsEnabled forwards the inner generator's capability report (see
// healthGenerator.NativeToolsEnabled).
func (g *runTraceGenerator) NativeToolsEnabled() bool {
	return domain.SupportsToolCalling(g.inner)
}

func (g *runTraceGenerator) GenerateWithTools(ctx context.Context, messages []domain.ModelMessage, tools []domain.ToolDefinition) (domain.ModelTurn, error) {
	tg, ok := g.inner.(domain.ToolCallingGenerator)
	if !ok {
		return domain.ModelTurn{}, fmt.Errorf("llm provider: run-traced %T does not implement native tool-calling", g.inner)
	}
	req := domain.GenerateRequest{Messages: messages, Tools: tools}
	id, start := g.begin(ctx, req)
	out, err := tg.GenerateWithTools(ctx, messages, tools)
	g.span(ctx, id, start, req,
		domain.GenerateResponse{Text: out.Text, ToolCalls: out.ToolCalls, StopReason: out.StopReason}, err)
	return out, err
}

func (g *runTraceGenerator) GenerateStream(ctx context.Context, prompt string) (<-chan domain.StreamChunk, error) {
	sg, ok := g.inner.(streamingInner)
	if !ok {
		return nil, fmt.Errorf("llm provider: run-traced %T does not implement streaming", g.inner)
	}
	req := domain.UserPrompt(prompt)
	id, start := g.begin(ctx, req)
	in, err := sg.GenerateStream(ctx, prompt)
	if err != nil {
		g.span(ctx, id, start, req, domain.GenerateResponse{}, err)
		return nil, err
	}
	return g.relay(ctx, id, start, req, in), nil
}

func (g *runTraceGenerator) GenerateRequest(ctx context.Context, req domain.GenerateRequest) (domain.GenerateResponse, error) {
	rg, ok := domain.AsRequestGenerator(g.inner)
	if !ok {
		return domain.GenerateResponse{}, fmt.Errorf("llm provider: run-traced %T does not implement GenerateRequest", g.inner)
	}
	id, start := g.begin(ctx, req)
	out, err := rg.GenerateRequest(ctx, req)
	g.span(ctx, id, start, req, out, err)
	return out, err
}

func (g *runTraceGenerator) StreamRequest(ctx context.Context, req domain.GenerateRequest) (<-chan domain.StreamChunk, error) {
	rg, ok := domain.AsRequestGenerator(g.inner)
	if !ok {
		return nil, fmt.Errorf("llm provider: run-traced %T does not implement StreamRequest", g.inner)
	}
	id, start := g.begin(ctx, req)
	in, err := rg.StreamRequest(ctx, req)
	if err != nil {
		g.span(ctx, id, start, req, domain.GenerateResponse{}, err)
		return nil, err
	}
	return g.relay(ctx, id, start, req, in), nil
}

// relay forwards a stream unchanged and records one span when it ends: the final
// chunk's Response when the client sent one, else the concatenated text deltas.
func (g *runTraceGenerator) relay(ctx context.Context, id string, start time.Time, req domain.GenerateRequest, in <-chan domain.StreamChunk) <-chan domain.StreamChunk {
	out := make(chan domain.StreamChunk, 64)
	go func() {
		defer close(out)
		var text strings.Builder
		var final *domain.GenerateResponse
		var usage domain.GenerateUsage
		for c := range in {
			text.WriteString(c.Text)
			if c.Response != nil {
				final = c.Response
			}
			if c.IsFinal {
				usage.Input, usage.Output, usage.CacheRead = c.UsagePromptTokens, c.UsageCompletionTokens, c.UsageCacheReadTokens
			}
			out <- c
		}
		resp := domain.GenerateResponse{Text: text.String(), Usage: usage}
		if final != nil {
			resp = *final
			if resp.Text == "" {
				resp.Text = text.String()
			}
		}
		// A stream carries no error field: a failed stream shows as an empty output.
		g.span(context.WithoutCancel(ctx), id, start, req, resp, nil)
	}()
	return out
}

func (g *runTraceGenerator) Capabilities() domain.Capabilities { return capabilitiesOf(g.inner) }

func (g *runTraceGenerator) RequestsEnabled() bool {
	_, ok := domain.AsRequestGenerator(g.inner)
	return ok
}
