package domain

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"
)

// RunTracer receives the decision-level trace of a run (ADR-0145 D10, the ADR-0139
// C6 run event log): every model call (inputs, tools offered, output), every tool
// call (arguments, result) and every step output, so a reader can replay what each
// call saw. Spans follow the OpenTelemetry GenAI semantic conventions
// (github.com/open-telemetry/semantic-conventions-genai, docs/gen-ai/client-inference.md
// and execute-tool.md, checked 2026-11-08): the span name is
// "{gen_ai.operation.name} {target}" and the attributes use the gen_ai.* names below;
// kernel facts the conventions have no name for use cambrian.*.
//
// Implementations must be safe for concurrent use, must never block a caller on
// slow I/O for long, and must never fail the call they observe: tracing is
// best-effort. Enabled by execution.trace.enabled (default off).
type RunTracer interface {
	Span(ctx context.Context, s TraceSpan)
}

// TraceSpan is one finished span, or the start event of one (Phase ==
// TracePhaseStart).
//
// Start events exist so a follower of the trace sees what is running NOW
// (T-20261109-live-run-viewer-watch-model-calls-prompts-tool): a call that
// records only when it ends is invisible while it runs, and a slow model call or
// tool call is exactly the one an operator wants to watch. The pair shares SpanID;
// the start event carries the request (what the call was sent) and
// "cambrian.phase": "start", the finished span is written exactly as before and
// completes the pair. A reader that wants finished spans only skips the events
// whose phase is "start".
type TraceSpan struct {
	// Name is "{gen_ai.operation.name} {target}", e.g. "chat claude-haiku-4-5",
	// "execute_tool mcp:fs/read_file", "invoke_agent worker-1".
	Name string
	// Kind is the OTel span kind: "CLIENT" for a call leaving the process, else "INTERNAL".
	Kind       string
	Start, End time.Time
	// Attributes are gen_ai.* / cambrian.* names to JSON-encodable values. The
	// tracer bounds long strings; callers pass the full content.
	Attributes map[string]any
	// Error is the failure message; "" is an OK span.
	Error string
	// SpanID joins a start event to its finished span. "" lets the tracer mint one
	// (a span with no start event). Use NewTraceSpanID for a pair.
	SpanID string
	// Phase is TracePhaseStart for the start event of a call; "" for the finished
	// span.
	Phase string
}

// TracePhaseStart marks a span's start event; TraceAttrPhase is the attribute it
// is written under.
const (
	TracePhaseStart = "start"
	TraceAttrPhase  = "cambrian.phase"
)

// NewTraceSpanID mints an OTel span id (8 random bytes, 16 hex chars).
func NewTraceSpanID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // crypto/rand.Read does not fail on supported platforms
	return hex.EncodeToString(b)
}

// StartTraceSpan writes the start event of s on t (when t is non-nil) and returns
// the span id the finished span must carry. s.Start is set to now when zero; the
// returned time is the start the finished span should report.
func StartTraceSpan(ctx context.Context, t RunTracer, s TraceSpan) (string, time.Time) {
	id := NewTraceSpanID()
	if s.Start.IsZero() {
		s.Start = time.Now()
	}
	if t != nil {
		s.SpanID, s.Phase = id, TracePhaseStart
		t.Span(ctx, s)
	}
	return id, s.Start
}

type traceAttrsKey struct{}

// WithTraceAttributes returns ctx carrying attributes every span recorded under it
// gets (a span's own attribute of the same name wins). Used by a caller that knows
// who a call is for (the lease, the agent, the plan step) when the layer that
// records the span does not, e.g. the agent-plane gateway around a model call.
// Merges with attributes already on ctx.
func WithTraceAttributes(ctx context.Context, attrs map[string]any) context.Context {
	if len(attrs) == 0 {
		return ctx
	}
	merged := make(map[string]any, len(attrs))
	for k, v := range TraceAttributesFromContext(ctx) {
		merged[k] = v
	}
	for k, v := range attrs {
		merged[k] = v
	}
	return context.WithValue(ctx, traceAttrsKey{}, merged)
}

// TraceAttributesFromContext returns the attributes WithTraceAttributes put on
// ctx (nil when none). Callers must not mutate the map.
func TraceAttributesFromContext(ctx context.Context) map[string]any {
	m, _ := ctx.Value(traceAttrsKey{}).(map[string]any)
	return m
}

// GenAI semantic-convention attribute names and operation values.
const (
	GenAIOperationName       = "gen_ai.operation.name"
	GenAIProviderName        = "gen_ai.provider.name"
	GenAIRequestModel        = "gen_ai.request.model"
	GenAIResponseModel       = "gen_ai.response.model"
	GenAIResponseFinish      = "gen_ai.response.finish_reasons"
	GenAIUsageInputTokens    = "gen_ai.usage.input_tokens"
	GenAIUsageOutputTokens   = "gen_ai.usage.output_tokens"
	GenAIUsageCacheReadInput = "gen_ai.usage.cache_read.input_tokens"
	GenAIInputMessages       = "gen_ai.input.messages"
	GenAIOutputMessages      = "gen_ai.output.messages"
	GenAISystemInstructions  = "gen_ai.system_instructions"
	GenAIToolDefinitions     = "gen_ai.tool.definitions"
	GenAIToolName            = "gen_ai.tool.name"
	GenAIToolCallID          = "gen_ai.tool.call.id"
	GenAIToolCallArguments   = "gen_ai.tool.call.arguments"
	GenAIToolCallResult      = "gen_ai.tool.call.result"
	GenAIConversationID      = "gen_ai.conversation.id"
	GenAIAgentName           = "gen_ai.agent.name"

	GenAIOpChat        = "chat"
	GenAIOpExecuteTool = "execute_tool"
	GenAIOpInvokeAgent = "invoke_agent"
)

// GenAITextMessage renders one message in the GenAI message shape
// ({role, parts:[{type:"text", content}]}).
func GenAITextMessage(role, text string) map[string]any {
	return map[string]any{"role": role, "parts": []map[string]any{{"type": "text", "content": text}}}
}

// GenAIMessages renders model messages in the GenAI message shape: text parts,
// tool_call parts on assistant turns and a tool_call_response part on tool turns.
func GenAIMessages(msgs []ModelMessage) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		var parts []map[string]any
		if m.Role == RoleTool {
			parts = append(parts, map[string]any{"type": "tool_call_response", "id": m.ToolCallID, "response": m.Content})
		} else if m.Content != "" {
			parts = append(parts, map[string]any{"type": "text", "content": m.Content})
		}
		for _, c := range m.ToolCalls {
			parts = append(parts, map[string]any{"type": "tool_call", "id": c.ID, "name": c.Name, "arguments": string(c.Arguments)})
		}
		out = append(out, map[string]any{"role": m.Role, "parts": parts})
	}
	return out
}
