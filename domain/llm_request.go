package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// The C1 LLM request contract (ADR-0139 D1, frozen in v2.0).
//
// Every adapter used to take one prompt string and send it as one user turn, so a
// system prompt, a cache breakpoint, a response schema and any generation option were
// impossible to express: they were text inside the prompt or they were dropped. This
// file is the structured request every adapter now implements. The older ports —
// Generator.Generate, ToolCallingGenerator.GenerateWithTools, LLMStreamer.GenerateStream
// — remain as thin shims over it (a prompt becomes Messages=[user prompt]), so there is
// one pathway per adapter rather than four.
//
// It reuses the ADR-0097 conversation types (ModelMessage, ModelToolCall,
// ToolDefinition, StopReason) instead of restating them (AGENTS.md §3.1).
//
// The honesty rule (refined C1, haiku-local probe 2026-11-04): a field a generator
// cannot honour is never dropped silently. It is reported on GenerateResponse.Ignored,
// and it is ErrUnsupported only when the request marked it strict — Options.Strict for
// max_tokens, sampling and stop; ResponseSchema.Strict for constrained decoding.

// ErrUnsupported is returned when a request requires (strictly) a field the generator
// cannot honour. Always wrapped with the field name: errors.Is(err, ErrUnsupported).
var ErrUnsupported = errors.New("llm: unsupported request field")

// UnsupportedField wraps ErrUnsupported with the field that triggered it.
func UnsupportedField(field, why string) error {
	if why == "" {
		return fmt.Errorf("%w: %s", ErrUnsupported, field)
	}
	return fmt.Errorf("%w: %s (%s)", ErrUnsupported, field, why)
}

// ErrSchemaValidation is returned when a reply still fails its ResponseSchema after
// the provider layer's single repair retry. It is a property of the ANSWER, not of
// the endpoint, so it never triggers provider fallback (ADR-0139 D1).
var ErrSchemaValidation = errors.New("llm: response failed its schema")

// Request field names, as they appear in GenerateResponse.Ignored and in
// ErrUnsupported messages. Stable strings: traces and tests match on them.
const (
	FieldSystem           = "system"
	FieldTools            = "tools"
	FieldToolChoice       = "tool_choice"
	FieldParallelTools    = "parallel_tool_calls"
	FieldResponseSchema   = "response_schema"
	FieldMaxTokens        = "max_tokens"
	FieldTemperature      = "temperature"
	FieldStop             = "stop"
	FieldCacheBreakpoints = "cache_breakpoints"
	FieldCacheTTL         = "cache_ttl"
)

// SystemBlock is one block of the system prompt. Cache marks the END of a stable
// prefix the provider may cache (an Anthropic cache_control breakpoint). The stable
// tier goes first and must not change between calls; volatile text belongs in
// Messages (ADR-0139 D2), which is what lets an implicitly-caching proxy cache it.
type SystemBlock struct {
	Text  string
	Cache bool
}

// ToolChoiceMode says how the model may use the offered tools.
type ToolChoiceMode string

const (
	// ToolChoiceAuto (the zero value) lets the model decide.
	ToolChoiceAuto ToolChoiceMode = ""
	// ToolChoiceNone forbids tool calls this turn.
	ToolChoiceNone ToolChoiceMode = "none"
	// ToolChoiceRequired forces at least one tool call.
	ToolChoiceRequired ToolChoiceMode = "required"
	// ToolChoiceTool forces the tool named in ToolChoice.Name.
	ToolChoiceTool ToolChoiceMode = "tool"
)

// ToolChoice constrains tool use. The zero value is "auto, parallel allowed", which
// is exactly what the pre-C1 tool path sent.
type ToolChoice struct {
	Mode ToolChoiceMode
	Name string // ToolChoiceTool only
	// DisableParallel asks for at most one tool call per turn.
	DisableParallel bool
}

// ResponseSchema asks for a JSON reply conforming to Schema (a JSON Schema object).
// Strict requires constrained decoding: a generator that can only best-effort it
// returns ErrUnsupported instead of a validated-and-repaired reply.
type ResponseSchema struct {
	Name   string
	Schema json.RawMessage
	Strict bool
}

// RequestOptions are the generation options of a GenerateRequest.
//
// Not GenerateOptions: that type is the agent-plane wire mirror of the proto's
// options (no field presence, so 0 means unset) and changes with the gateway in
// wave 1c. Temperature is a pointer here because 0 is a meaningful temperature.
type RequestOptions struct {
	// MaxTokens caps output tokens; 0 = the generator's configured default.
	MaxTokens int
	// Temperature; nil = the provider's default.
	Temperature *float32
	Stop        []string
	// Strict turns an unhonoured max_tokens, temperature or stop into ErrUnsupported
	// instead of an entry in GenerateResponse.Ignored.
	Strict bool
}

// CacheTTL is the requested lifetime of cache breakpoints, chosen by caller pacing
// (ADR-0139 D1): long for human-paced sessions, short for daemons, cron and subagents.
type CacheTTL string

const (
	CacheTTLDefault CacheTTL = ""      // the provider's default (Anthropic: 5m)
	CacheTTLShort   CacheTTL = "short" // 5 minutes
	CacheTTLLong    CacheTTL = "long"  // 1 hour
)

// GenerateRequest is one structured model call (C1).
type GenerateRequest struct {
	System         []SystemBlock
	Messages       []ModelMessage
	Tools          []ToolDefinition
	ToolChoice     ToolChoice
	ResponseSchema *ResponseSchema
	Options        RequestOptions
	CacheTTL       CacheTTL
	// ProviderExtensions is the evolvable escape hatch: opaque values keyed
	// "<provider>:<field>" ("openai:thinking", "anthropic:metadata", "gemini:...";
	// "ollama:" or "openai:" for Ollama). An adapter sends the keys addressed to it
	// as top-level request-body fields and ignores every other key, so an extension
	// written for one provider never reaches another after a fallback.
	ProviderExtensions map[string]json.RawMessage
}

// UserPrompt is the request the legacy single-string ports shim onto.
func UserPrompt(prompt string) GenerateRequest {
	return GenerateRequest{Messages: []ModelMessage{UserMessage(prompt)}}
}

// GenerateUsage is the usage a provider reported for one call.
//
// Input counts EVERY input token, cached or not — the same meaning
// TokenUsage.PromptTokens has across the spend ledger (ADR-0131 D2) — and CacheRead
// and CacheWrite are breakdowns of it, never additions.
type GenerateUsage struct {
	Input      int
	Output     int
	CacheRead  int
	CacheWrite int
	// CacheWriteKnown is false when the provider does not report cache writes
	// (OpenAI-compatible endpoints), so CacheWrite=0 means "unknown", not "none".
	CacheWriteKnown bool
	// Estimated marks a len/4 fallback rather than a provider report.
	Estimated bool
}

// GenerateResponse is the result of one GenerateRequest.
type GenerateResponse struct {
	Text       string
	ToolCalls  []ModelToolCall
	StopReason StopReason
	Usage      GenerateUsage
	// Ignored names every request field (Field* constants) the generator did not
	// honour, so a degraded call is visible in traces instead of silent.
	Ignored []string
	// Model is the model id the provider says served the call.
	Model string
}

// Turn is the ADR-0097 view of the response, for the native tool-calling shim.
func (r GenerateResponse) Turn() ModelTurn {
	return ModelTurn{Text: r.Text, ToolCalls: r.ToolCalls, StopReason: r.StopReason}
}

// Level is how far a generator honours one request field.
type Level string

const (
	// LevelEnforced: the provider guarantees it (constrained decoding, a hard cap).
	LevelEnforced Level = "enforced"
	// LevelBestEffort: sent, but not guaranteed. A best-effort response schema is
	// validated and repaired once by the provider layer.
	LevelBestEffort Level = "best_effort"
	// LevelUnsupported (and the zero value): not sent; reported in Ignored.
	LevelUnsupported Level = "unsupported"
)

// Sent reports whether a field at this level reaches the wire.
func (l Level) Sent() bool { return l == LevelEnforced || l == LevelBestEffort }

// Valid reports whether l is one of the three named levels.
func (l Level) Valid() bool {
	return l == LevelEnforced || l == LevelBestEffort || l == LevelUnsupported
}

// Capabilities declares, per field, what a generator honours. It is DECLARED (by the
// adapter, overridable per generator in config), never probed per call: a per-call
// probe makes behaviour depend on the first request of a session.
type Capabilities struct {
	SystemRole       Level
	Tools            Level
	ToolChoice       Level
	ParallelTools    Level
	ResponseSchema   Level
	MaxTokens        Level
	Sampling         Level // temperature
	Stop             Level
	CacheBreakpoints Level
	Images           Level
}

// RequestGenerator is the C1 port every adapter implements.
//
// StreamRequest's final chunk (IsFinal) carries the complete GenerateResponse in
// StreamChunk.Response — tool calls, stop reason, usage and Ignored — so a streaming
// caller loses nothing a non-streaming one gets.
type RequestGenerator interface {
	GenerateRequest(ctx context.Context, req GenerateRequest) (GenerateResponse, error)
	StreamRequest(ctx context.Context, req GenerateRequest) (<-chan StreamChunk, error)
	Capabilities() Capabilities
}

// RequestGeneratorReporter lets a decorator that HAS the RequestGenerator methods say
// its inner does not — same shape, and same reason, as ToolCallingReporter.
type RequestGeneratorReporter interface {
	RequestsEnabled() bool
}

// AsRequestGenerator returns v as a RequestGenerator when it genuinely is one,
// through any decorator that forwards the capability. Decorators MUST forward both
// the methods and the report; forwarding one without the other either hides the
// capability or claims it for an inner that lacks it.
func AsRequestGenerator(v any) (RequestGenerator, bool) {
	rg, ok := v.(RequestGenerator)
	if !ok {
		return nil, false
	}
	if r, ok := rg.(RequestGeneratorReporter); ok && !r.RequestsEnabled() {
		return nil, false
	}
	return rg, true
}
