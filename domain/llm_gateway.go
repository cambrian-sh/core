package domain

import (
	"context"
	"time"
)

// BudgetLease is a per-STEP LLM-budget credential issued by the Substrate at step dispatch,
// encoding (planID, stepIndex, allocatedTraitModelID, tokenLimit). ADR-0018.
//
// Named BudgetLease, not SessionToken (its historical name), because it is neither a
// conversation Session (ADR-0084) nor a login session — it is a short-lived spend lease on
// one step's model call. The lease is opaque to the cognitive agent — it cannot be forged,
// transferred, or reused. The mutable accounting state lives server-side in BudgetLeaseState.
// (The wire key `_session_token_id` and the proto field `session_token_id` keep their names
// for contract stability; only the Go domain vocabulary is corrected.)
type BudgetLease struct {
	ID string
}

// HandoffKeyBudgetOverrun is the Handoff.Context key on which a step function
// reports that the gateway's Pass-1 cut-off fired for the step's lease
// (BudgetLeaseState.BudgetOverrun, reconciled by Complete). The DAGExecutor
// reads it when it writes the step's TaskEvent — the one place TaskEvents are
// written — so TaskEvent.BudgetOverrun is server-authoritative, never
// agent-reported (ADR-0018;
// T-20261031-taskevent-budgetoverrun-is-written-by-no-kernel). Same `_`
// convention as `_session_token_id`: kernel-internal, never agent-settable.
const HandoffKeyBudgetOverrun = "_budget_overrun"

// Step-usage handoff keys: the step function stamps the step lease's reconciled
// provider usage (the gateway's Complete — every agent stream and tool turn of the
// step, plus the agentic retrieval chain charged back to it) on the step response,
// and the DAGExecutor prefers it over its len/4 estimate when it writes the
// step's TaskEvent (ADR-0131 D2; T-20261101-step-taskevents-still-bill-len-4-although-the).
// Kernel-internal like HandoffKeyBudgetOverrun: the step function deletes any
// agent-supplied value before stamping, and the executor consumes them.
const (
	HandoffKeyStepPromptTokens     = "_step_prompt_tokens"
	HandoffKeyStepCompletionTokens = "_step_completion_tokens"
	HandoffKeyStepCacheReadTokens  = "_step_cache_read_tokens"
	// HandoffKeyStepUsageEstimated is "true" when part of the stamped usage is the
	// gateway's len/4 fallback (the provider reported none for some call).
	HandoffKeyStepUsageEstimated = "_step_usage_estimated"
)

// BudgetLeaseState holds the server-side mutable accounting state for a single BudgetLease.
// Keyed by the lease's opaque UUID in a server-side map.
type BudgetLeaseState struct {
	StepAllocation   StepAllocation
	TokenLimit       int // maps to Step.MaxEnergy (ADR-0011)
	ConsumedTokens   int // running real-time estimate from Pass 1
	ActualTokensUsed int // reconciled at stream close via Pass 2
	// BudgetOverrun records that Pass 1 cut the stream at the 90% cut-off of
	// TokenLimit (ADR-0018). Set by StreamChunks, read back by Complete.
	BudgetOverrun bool
	// PromptTokensUsed / CacheReadTokensUsed split ActualTokensUsed (the total)
	// into its input share and the cached part of that input, summed over every
	// call made under the lease (ADR-0131 D2). UsageEstimated is true when at
	// least one of those calls had no provider usage and was counted by the
	// len/4 fallback.
	PromptTokensUsed    int
	CacheReadTokensUsed int
	// CacheWriteTokensUsed is the part of PromptTokensUsed written to the
	// provider's prompt cache (ENG-126; zero when the provider reports none).
	CacheWriteTokensUsed int
	UsageEstimated       bool
	ExpiresAt            time.Time // keepalive TTL — refreshed on each chunk
	// LastActivityAt is when a model call (or tracked tool/recall call) under the
	// lease last started, streamed a chunk or returned.
	LastActivityAt time.Time
	// InFlightCalls counts the calls running under the lease right now: model calls,
	// and the kernel-side tool calls and recalls the lease's holder made
	// (SubstrateLLMGateway.TrackLeaseWork). A unary call (GenerateWithTools)
	// reports nothing until it returns, so "a call is in flight" is the only
	// liveness signal a long generation gives; the chat stall watchdog reads it
	// through SubstrateLLMGateway.ConversationActivity
	// (T-20261105-slow-model-reliability-hardcoded-20-s) and the plan idle watchdog
	// through RunActivity (T-20261105-owner-decision-needed-120-s-plan-timeout-ms).
	// Server-side only.
	InFlightCalls int
	// LastFault is the class of the last failed model call under the lease, cleared by
	// a later successful one (ENG-2124). The chat lane reads it when a turn fails, so
	// the person is told "the AI service is busy" rather than shown the provider's
	// error — whatever the worker that made the call reported. Server-side only.
	LastFault ProviderFault
	// Binding is the execution identity the kernel attached at dispatch (session, run,
	// step, agent). It is what lets a transport resolve an agent's opaque lease back to
	// the session that owns it, instead of trusting the agent's own header. Written
	// server-side only; the zero value means "never bound". See LeaseBinding.
	Binding LeaseBinding
	// Background marks a lease minted for a background organ (verification): its
	// calls are booked to the bound session but never refused at that session's
	// spend ceiling (ADR-0131, owner decision 2026-11-01). Set by the gateway at
	// Acquire from the domain.WithBackgroundSpend ctx marker — never from the
	// wire — and deliberately NOT part of Binding, which BindLease replaces whole.
	Background bool
	// Tally is the per-request LLM usage tally (LLMUsageTally) of the ctx the lease
	// was acquired in — e.g. the QueryMemory whose agentic retrieval chain minted
	// it — so calls an agent makes under the lease over its own RPC are counted to
	// the request that started them. Set by the gateway at Acquire, never from the
	// wire, and like Background outside Binding. Nil = no request is counting.
	Tally *LLMUsageTally
}

// StepAllocation records the top-3 TraitModel candidates (winner + 2 fallbacks)
// for a given step, produced by the Dispatcher at plan time.
type StepAllocation struct {
	Winner    AgentDefinition
	Fallbacks [2]AgentDefinition
}

// GenerateOptions carries generation parameters that the agent sends to the
// Substrate via GenerateViaModelStream.
type GenerateOptions struct {
	MaxTokens     int32
	Temperature   float32
	StopSequences []string
}

// StreamChunk represents a single token group streamed from an LLM provider.
// On the final chunk, IsFinal=true and usage fields may be non-zero if the provider
// includes a usage field in the completion response (OpenAI, Anthropic).
// When the provider omits usage (Ollama streaming), usage fields remain zero
// and ReconcileTokens falls back to the len/4 estimate, flagged Estimated (ADR-0131 D2).
type StreamChunk struct {
	Text                  string
	IsFinal               bool
	UsagePromptTokens     int // non-zero only on final chunk with provider usage
	UsageCompletionTokens int // non-zero only on final chunk with provider usage
	UsageTotalTokens      int // non-zero only on final chunk with provider usage
	// UsageCacheReadTokens is the part of UsagePromptTokens the provider served
	// from its prompt cache (Anthropic cache_read_input_tokens, OpenAI
	// prompt_tokens_details.cached_tokens, Gemini cachedContentTokenCount).
	// Already INCLUDED in UsagePromptTokens; carried separately so the ADR-0131
	// spend ledger can report cache_read. Zero when the provider says nothing.
	UsageCacheReadTokens int
	// Response is set on the FINAL chunk of a RequestGenerator.StreamRequest (ADR-0139
	// C1): the whole call's text, tool calls, stop reason, usage and Ignored fields.
	// Nil on every other chunk and on the legacy GenerateStream path, whose consumers
	// read only the fields above.
	Response *GenerateResponse
	// ToolCall is one fragment of a streamed tool call (ADR-0139 amendment
	// 2026-11-06, live step-output stream): the adapters that stream tool-call
	// arguments (Anthropic input_json_delta, OpenAI-compatible tool_calls deltas)
	// emit it so the gateway can forward a final-answer argument live. Text is empty
	// on such a chunk. The complete calls still arrive on the final chunk's
	// Response; a consumer that ignores ToolCall loses nothing.
	ToolCall *ToolCallDelta
	// Err is the typed error behind a final "stream_error: …" chunk (the Text
	// convention is unchanged for consumers that read only Text). It lets the
	// fallback policy and the breaker tell a request fault (ErrContextOverflow)
	// from an endpoint failure after a stream crossed a channel.
	Err error
}

// ToolCallDelta is one streamed fragment of a model's tool call. Index identifies
// the call within the turn (Anthropic's content-block index, OpenAI's tool_calls
// index); ID and Name are set on the fragment that opens the call and may repeat;
// ArgumentsDelta is the next piece of the call's JSON arguments text.
type ToolCallDelta struct {
	Index          int
	ID             string
	Name           string
	ArgumentsDelta string
}

// ModelStreamDelta is what a streamed model call reports to a live observer while
// the call runs (llm.Provider.GenerateOnObserved): a text delta, a tool-call
// fragment, or Restarted — the call failed over to another generator, so every
// delta reported so far belongs to an abandoned attempt.
type ModelStreamDelta struct {
	Text      string
	ToolCall  *ToolCallDelta
	Restarted bool
}

// LLMStreamer is a provider client that can stream LLM responses.
// Implemented by OllamaClient, OpenAIClient, AnthropicClient.
type LLMStreamer interface {
	GenerateStream(ctx context.Context, prompt string) (<-chan StreamChunk, error)
}
