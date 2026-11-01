package llm

import (
	"github.com/cambrian-sh/core/domain"
)

// ReconcileTokens applies dual-pass token accounting (ADR-0018) to one streamed
// call. Pass 1 was per-chunk EstimateTokens enforcement (handled by the
// LLMGateway). Pass 2 is post-stream reconciliation:
//   - When the stream carried provider usage (UsageTotalTokens > 0 on the chunk
//     passed in, see StreamUsage), that usage is the answer, cache reads included.
//   - When the provider omits usage (Ollama without eval counts, a stream cut by
//     Pass 1 before the usage chunk arrived), the call is counted by the len/4
//     fallback over BOTH the prompt and the response, flagged Estimated
//     (ADR-0131 D2). Counting the prompt matters: input dominates cost for most
//     prompts, and the old completion-only fallback billed it as zero.
func ReconcileTokens(prompt string, usage domain.StreamChunk, fullResponseText string) TokenUsage {
	if usage.UsageTotalTokens > 0 {
		return TokenUsage{
			PromptTokens:     usage.UsagePromptTokens,
			CompletionTokens: usage.UsageCompletionTokens,
			TotalTokens:      usage.UsageTotalTokens,
			CacheReadTokens:  usage.UsageCacheReadTokens,
		}
	}
	return EstimateUsage(prompt, fullResponseText)
}

// StreamUsage folds the usage a provider reported across a stream's chunks into
// one chunk-shaped value for ReconcileTokens. The LAST chunk that carries usage
// wins: OpenAI sends it on a trailing chunk after finish_reason, Ollama and
// Gemini on the final chunk, and the Anthropic streamer merges message_start's
// input count into its message_delta chunk — so the last report is the complete
// one.
type StreamUsage struct {
	last domain.StreamChunk
}

// Observe records chunk's usage if it carries any.
func (s *StreamUsage) Observe(chunk domain.StreamChunk) {
	if chunk.UsageTotalTokens > 0 {
		s.last = chunk
	}
}

// Chunk returns the folded usage (the zero chunk when none carried any).
func (s *StreamUsage) Chunk() domain.StreamChunk { return s.last }
