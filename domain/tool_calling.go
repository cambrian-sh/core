package domain

import "context"

// Native tool-calling (ADR-0097 Phase A).
//
// The kernel's agent loop asks a model for JSON in text and infers what the model
// meant from whether that text parses. That inference is the root of a whole failure
// class: a model narrating its next step is byte-identical to a model finishing, and
// a code-fenced classification killed the router outright. Every provider we talk to
// — Anthropic, OpenAI, Gemini, Ollama — reports the distinction as a structured field
// instead, and we were discarding it at the adapter.
//
// Phase A makes that field reachable INSIDE the kernel. It does not change the agent
// plane: agents still cannot receive a structured tool call, because the proto carries
// text only. See ADR-0097 D6.

// StopReason is the normalized reason a generation ended, mapped at the adapter so no
// caller ever learns a provider's vocabulary.
//
// The set follows Anthropic's, which is the most complete of the four.
type StopReason string

const (
	// StopToolUse — the model wants a tool run. NOT finished.
	StopToolUse StopReason = "tool_use"
	// StopEndTurn — the model finished of its own accord. The ONLY finished state.
	StopEndTurn StopReason = "end_turn"
	// StopMaxTokens — output limit hit. The response is truncated, not finished.
	StopMaxTokens StopReason = "max_tokens"
	// StopSequence — a caller-supplied stop sequence fired.
	StopSequence StopReason = "stop_sequence"
	// StopRefusal — the model declined.
	StopRefusal StopReason = "refusal"
	// StopUnknown — the provider reported something we do not recognise.
	//
	// It deliberately does NOT mean "finished". Treating an unrecognised signal as
	// completion is the exact mistake this package exists to stop making: a new or
	// mis-mapped provider value must degrade to "keep going", never to "done".
	StopUnknown StopReason = "unknown"
)

// IsFinished reports whether generation completed of the model's own accord.
//
// A whitelist, per Anthropic's guidance — "when stop_reason is NOT end_turn, treat the
// response as incomplete". Written as a method rather than left to each caller so the
// rule has one home and cannot drift into a blacklist somewhere.
func (s StopReason) IsFinished() bool { return s == StopEndTurn }

// ToolDefinition is one tool offered to the model. Parameters is a JSON Schema object,
// carried as raw bytes because it is authored elsewhere (agent manifests, the kernel's
// tool registry) and this layer only forwards it.
type ToolDefinition struct {
	Name        string
	Description string
	Parameters  []byte
}

// ModelToolCall is one tool invocation the model asked for.
//
// Named ModelToolCall, not ToolCall: domain.ToolCall already exists and means the
// kernel EXECUTING an authorized call (ADR-0039). These are different halves of the
// same story — the model requests, the kernel executes — and collapsing them into one
// name would blur an authorization boundary.
type ModelToolCall struct {
	// ID is the provider's call id. It MUST be echoed back on the corresponding tool
	// result — OpenAI and Anthropic both correlate on it, and a synthesized id is
	// rejected. Never generate one locally.
	ID string
	// Name is the tool the model chose.
	Name string
	// Arguments is the raw JSON argument object, unparsed. Providers return it as a
	// JSON *string*; validating it against the tool's schema belongs to the caller
	// that owns the tool, not to the transport.
	Arguments []byte
}

// Message roles for ModelMessage.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// ModelMessage is one turn of the provider conversation (ADR-0097 D8).
//
// Native tool-calling is conversational. The model answers with an ASSISTANT turn
// carrying ToolCalls; the caller must send that turn back unchanged, followed by one
// TOOL turn per call carrying the result under the same ToolCallID. Only then can the
// model see that its call happened and what came back.
//
// Sending a single user turn each round — which is what the first cut of Phase B did —
// makes every round a fresh conversation in which the model has never called anything.
// It re-explores instead of progressing, and reads any narrated history as a report
// about someone else. That is not a tuning problem; it is half a protocol.
type ModelMessage struct {
	Role       string
	Content    string
	ToolCalls  []ModelToolCall // assistant turns only
	ToolCallID string          // tool turns only — the call this answers
}

// UserMessage is the common single-turn case.
func UserMessage(content string) ModelMessage {
	return ModelMessage{Role: RoleUser, Content: content}
}

// ModelTurn is one generation turn: what the model said, what it wants run, and why
// it stopped. (domain.ToolCallResponse is taken, and means the RESULT of executing a
// tool — see ModelToolCall on why the two are kept apart.)
type ModelTurn struct {
	Text       string
	ToolCalls  []ModelToolCall
	StopReason StopReason
}

// ShouldContinue reports whether the loop must run another turn.
//
// It checks BOTH signals — the declared stop reason AND whether any tool call is
// actually present — because providers disagree. Gemini and LiteLLM are documented to
// return finish_reason "stop" on responses that DO carry tool calls (opencode #14972),
// so a loop trusting the declared reason alone executes one tool and halts. Trust the
// action over the narration; it is the same principle the agent loop applies to models,
// applied here to providers. ADR-0097 D4.
func (r ModelTurn) ShouldContinue() bool {
	if len(r.ToolCalls) > 0 {
		return true
	}
	return !r.StopReason.IsFinished()
}

// ToolCallingGenerator is the OPTIONAL native tool-calling capability a Generator may
// implement (ADR-0097 D1).
//
// Deliberately separate from Generator rather than widening it: Generator is referenced
// across 22 files with nine implementers plus the premium trace wrapper, most of which —
// the failover wrapper, the circuit breaker, the price ledger — have nothing to do with
// tools. Callers type-assert; a generator that does not implement this keeps today's
// behaviour untouched.
//
// Decorators MUST forward the assertion (see SupportsToolCalling). A decorator that
// forwards Generate but not this silently downgrades a capable generator to the text
// path, which is the same silent-wiring shape that has cost this codebase a day at a
// time. There is a forwarding test per decorator for exactly that reason.
type ToolCallingGenerator interface {
	// GenerateWithTools runs one turn of a tool-calling CONVERSATION. The caller owns
	// the message list and must carry prior assistant turns and their tool results
	// forward — see ModelMessage.
	GenerateWithTools(ctx context.Context, messages []ModelMessage, tools []ToolDefinition) (ModelTurn, error)
}

// ToolCallingReporter lets a generator that HAS the method still declare the
// capability unavailable — an OpenAI-COMPATIBLE endpoint is not necessarily
// tool-capable, and that is config, not code shape (ADR-0097 D2).
//
// An earlier attempt hid the capability by wrapping the client in a value exposing
// only Generator. That worked, and also silently hid STREAMING, because embedding an
// interface narrows the method set to exactly that interface. A wrapper that hides
// every optional capability in order to hide one is the same silent-drop shape this
// ADR is trying to eliminate. Reporting is narrower and cannot take anything else
// down with it.
type ToolCallingReporter interface {
	NativeToolsEnabled() bool
}

// SupportsToolCalling reports whether v can do native tool-calling, through any
// decorator that forwards the capability.
//
// A value implementing ToolCallingGenerator is capable UNLESS it reports otherwise.
// Decorators must forward BOTH the call and the report; forwarding one without the
// other over-reports a disabled inner, so each is tested per decorator.
//
// Takes `any`, not Generator: this is a capability probe, and the same clients are
// held as domain.LLMStreamer on the agent-plane gateway. Constraining the parameter
// would exclude a value that genuinely has the capability, for no benefit.
func SupportsToolCalling(v any) bool {
	tg, ok := v.(ToolCallingGenerator)
	if !ok {
		return false
	}
	if r, ok := tg.(ToolCallingReporter); ok {
		return r.NativeToolsEnabled()
	}
	return true
}
