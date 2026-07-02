package llm

import (
	"github.com/cambrian-sh/cambrian-runtime/domain"
)

// ReconcileTokens applies dual-pass token accounting (ADR-0018).
// Pass 1 was per-chunk EstimateTokens enforcement (handled by the LLMGateway).
// Pass 2 is post-stream exact reconciliation:
//   - When the final chunk carries provider usage (UsageTotalTokens > 0), use it.
//   - When the provider omits usage (Ollama streaming), fall back to
//     EstimateTokens(fullResponseText).
func ReconcileTokens(estimatedCount int, finalChunk domain.StreamChunk, fullResponseText string) TokenUsage {
	if finalChunk.UsageTotalTokens > 0 {
		return TokenUsage{
			PromptTokens:     finalChunk.UsagePromptTokens,
			CompletionTokens: finalChunk.UsageCompletionTokens,
			TotalTokens:      finalChunk.UsageTotalTokens,
		}
	}
	total := EstimateTokens(fullResponseText)
	return TokenUsage{
		PromptTokens:     0,
		CompletionTokens: total,
		TotalTokens:      total,
	}
}
