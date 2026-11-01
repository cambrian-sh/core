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

// BudgetLeaseState holds the server-side mutable accounting state for a single BudgetLease.
// Keyed by the lease's opaque UUID in a server-side map.
type BudgetLeaseState struct {
	StepAllocation   StepAllocation
	TokenLimit       int // maps to Step.MaxEnergy (ADR-0011)
	ConsumedTokens   int // running real-time estimate from Pass 1
	ActualTokensUsed int // reconciled at stream close via Pass 2
	// BudgetOverrun records that Pass 1 cut the stream at the 90% cut-off of
	// TokenLimit (ADR-0018). Set by StreamChunks, read back by Complete.
	BudgetOverrun  bool
	ExpiresAt      time.Time // keepalive TTL — refreshed on each chunk
	LastActivityAt time.Time
	// Binding is the execution identity the kernel attached at dispatch (session, run,
	// step, agent). It is what lets a transport resolve an agent's opaque lease back to
	// the session that owns it, instead of trusting the agent's own header. Written
	// server-side only; the zero value means "never bound". See LeaseBinding.
	Binding LeaseBinding
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
// and ReconcileTokens falls back to EstimateTokens(fullResponseText).
type StreamChunk struct {
	Text                  string
	IsFinal               bool
	UsagePromptTokens     int // non-zero only on final chunk with provider usage
	UsageCompletionTokens int // non-zero only on final chunk with provider usage
	UsageTotalTokens      int // non-zero only on final chunk with provider usage
}

// LLMStreamer is a provider client that can stream LLM responses.
// Implemented by OllamaClient, OpenAIClient, AnthropicClient.
type LLMStreamer interface {
	GenerateStream(ctx context.Context, prompt string) (<-chan StreamChunk, error)
}
