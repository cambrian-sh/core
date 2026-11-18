package llm

import (
	"math"
	"math/bits"

	"github.com/cambrian-sh/core/internal/config"
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

// EstimateTokens returns a token count for text without a tokenizer.
//
// It is content-aware (T-20261112-pre-send-context-window-check-per-model-with):
// ordinary text counts at config.CharsPerToken (4) characters per token, while a
// high-entropy run — a maximal run of at least config.HighEntropyMinRun base64 /
// base64url / hex characters mixing at least two of lower case, upper case and
// digits — counts at config.HighEntropyCharsPerToken (1.3). Plain len/4
// under-counted base64 2-3x, so a prompt full of encoded payloads estimated well
// under a context window it overflowed. One pass over the bytes; no allocation.
// The SDK's context_engine.estimate_tokens implements the same rule.
func EstimateTokens(text string) int {
	if len(text) == 0 {
		return 0
	}
	normal, entropy := splitEntropy(text)
	tokens := normal / config.CharsPerToken
	if entropy > 0 {
		tokens += int(math.Ceil(float64(entropy) / config.HighEntropyCharsPerToken))
	}
	if tokens == 0 {
		tokens = 1
	}
	return tokens
}

// splitEntropy returns how many bytes of text are ordinary and how many lie in
// high-entropy runs (see EstimateTokens).
func splitEntropy(text string) (normal, entropy int) {
	start := -1
	var classes uint8
	flush := func(end int) {
		if start < 0 {
			return
		}
		if n := end - start; n >= config.HighEntropyMinRun && bits.OnesCount8(classes) >= 2 {
			entropy += n
		}
		start, classes = -1, 0
	}
	for i := 0; i < len(text); i++ {
		c := text[i]
		var class uint8
		switch {
		case c >= 'a' && c <= 'z':
			class = 1
		case c >= 'A' && c <= 'Z':
			class = 2
		case c >= '0' && c <= '9':
			class = 4
		case c == '+' || c == '/' || c == '=' || c == '-' || c == '_':
			class = 0 // in the alphabet, but no class of its own
		default:
			flush(i)
			continue
		}
		if start < 0 {
			start = i
		}
		classes |= class
	}
	flush(len(text))
	return len(text) - entropy, entropy
}
