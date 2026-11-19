package llm

import (
	"github.com/cambrian-sh/core/internal/tokenest"
)

// TokenUsage holds the token counts from an LLM response.
type TokenUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	// CacheReadTokens is the part of PromptTokens the provider served from its
	// prompt cache. Already included in PromptTokens (ADR-0131 D2).
	CacheReadTokens int
	// CacheWriteTokens is the part of PromptTokens the provider wrote to its
	// prompt cache on this call (Anthropic cache_creation_input_tokens; billed
	// above the plain input price). Already included in PromptTokens. Zero when
	// the provider does not report writes (OpenAI-compatible endpoints), so a
	// zero is "none reported", not "none written" (ENG-126).
	CacheWriteTokens int
	// Estimated is true when the counts come from the len/4 fallback because the
	// provider returned no usage (ADR-0131 D2: real usage first, estimate only as
	// a flagged fallback). A sum is Estimated when any of its parts is.
	Estimated bool
	// BudgetOverrun is set by the gateway's Complete when its Pass-1 cut-off fired
	// for the lease (ADR-0018). Providers never set it.
	BudgetOverrun bool
}

// IsZero reports whether u carries no token counts at all — the shape a provider
// response without a usage block parses to.
func (u TokenUsage) IsZero() bool {
	return u.PromptTokens == 0 && u.CompletionTokens == 0 && u.TotalTokens == 0
}

// Add returns the field-wise sum of u and v. Estimated is sticky; BudgetOverrun
// is OR-ed.
func (u TokenUsage) Add(v TokenUsage) TokenUsage {
	return TokenUsage{
		PromptTokens:     u.PromptTokens + v.PromptTokens,
		CompletionTokens: u.CompletionTokens + v.CompletionTokens,
		TotalTokens:      u.TotalTokens + v.TotalTokens,
		CacheReadTokens:  u.CacheReadTokens + v.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens + v.CacheWriteTokens,
		Estimated:        u.Estimated || v.Estimated,
		BudgetOverrun:    u.BudgetOverrun || v.BudgetOverrun,
	}
}

// EstimateUsage is the len/4 fallback for one call, flagged Estimated. It is the
// one place an estimated usage row is built, so "estimated" always means the
// same thing (ADR-0131 D2).
func EstimateUsage(prompt, response string) TokenUsage {
	in, out := EstimateTokens(prompt), EstimateTokens(response)
	return TokenUsage{PromptTokens: in, CompletionTokens: out, TotalTokens: in + out, Estimated: true}
}

// CalculateCost computes the estimated cost of a model call.
// inputCostPer1M is the cost per 1 million input/prompt tokens.
// outputCostPer1M is the cost per 1 million output/completion tokens.
func CalculateCost(usage TokenUsage, inputCostPer1M, outputCostPer1M float64) float64 {
	inputCost := (float64(usage.PromptTokens) / 1_000_000.0) * inputCostPer1M
	outputCost := (float64(usage.CompletionTokens) / 1_000_000.0) * outputCostPer1M
	return inputCost + outputCost
}

// EstimateTokens returns a token count for text without a tokenizer: the
// kernel's one content-aware estimator, tokenest.Estimate (see there for the rule;
// it lives in a leaf package so internal/chat budgets with it too, ENG-2127).
func EstimateTokens(text string) int { return tokenest.Estimate(text) }
